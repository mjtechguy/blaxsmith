import { useMemo, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, LogOut, Plus, Users as UsersIcon, UserX } from "lucide-react";
import { CollectionTable, useUrlView, type GridColumn } from "../data-table";
import type { OrgMember } from "../gen/blaxsmith/api/v1/users_pb";
import { PageHeader, PageShell } from "../page";
import { EmptyState, Timestamp } from "../ui";
import { canManage } from "../users";
import { BulkMemberDialog, memberStatusBadge, roleLabel } from "../users-ui";
import { useScope } from "../workspace-ui";
import { listMembersPage, membersPageKey } from "../workspace";

export const Route = createFileRoute("/admin/users/")({ component: Users });

const defaults = { sort: [{ id: "role", desc: false }], size: 20 };

function Users() {
  const { session, scope } = useScope();
  const self = session.data?.principalId || "";
  const [view, setView] = useUrlView(defaults);
  const [bulk, setBulk] = useState<{ kind: "disable" | "revoke"; members: OrgMember[]; clear: () => void } | null>(null);
  const members = useQuery({ queryKey: membersPageKey(scope, view), enabled: Boolean(scope), placeholderData: keepPreviousData, queryFn: ({ signal }) => listMembersPage(view, signal) });
  const columns = useMemo<GridColumn<OrgMember>[]>(() => [
    { id: "username", accessorKey: "username", header: "Member", enableHiding: false, cell: ({ row }) => <Link className="row-link" to="/admin/users/$principalId" params={{ principalId: row.original.principalId }}>
      <span className="task-stage"><strong>{row.original.displayName || row.original.username}</strong><small>{row.original.username}{row.original.principalId === self ? " · you" : ""}</small></span></Link> },
    { id: "role", accessorKey: "role", header: "Role", cell: ({ row }) => <span className="state-badge">{roleLabel[row.original.role] ?? row.original.role}</span> },
    { id: "status", accessorKey: "status", header: "Status", enableSorting: false, cell: ({ row }) => <span className={`state-badge ${memberStatusBadge[row.original.status] ?? ""}`}>{row.original.status}</span> },
    { id: "sessions", accessorKey: "activeSessions", header: "Sessions", enableSorting: false, cell: ({ row }) => row.original.activeSessions },
    { id: "lastLogin", accessorKey: "lastLoginAt", header: "Last login", cell: ({ row }) => row.original.lastLoginAt ? <Timestamp value={row.original.lastLoginAt} /> : <span className="muted">Never</span> },
    { id: "created", accessorKey: "createdAt", header: "Added", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
    { id: "open", header: "", enableSorting: false, enableHiding: false, cell: ({ row }) => <Link className="text-action" to="/admin/users/$principalId" params={{ principalId: row.original.principalId }}>Manage <ArrowRight size={13} aria-hidden="true" /></Link> },
  ], [self]);
  const denied = members.isError && ConnectError.from(members.error).code === Code.PermissionDenied;
  const selectable = (m: OrgMember) => m.principalId !== self && canManage(session.data, m.role);

  return <PageShell>
    <PageHeader title="Users" description="Organization members, their roles, and account access. Invitations and resets use one-time links you copy; no email is sent."
      actions={<Link className="primary-button" to="/admin/users/new"><Plus size={15} aria-hidden="true" /> Invite user</Link>} />
    <section className="table-section" aria-label="Members">
      <CollectionTable id="admin-users" label="Members" noun="members" columns={columns} data={members.data?.members ?? []} getRowId={(m) => m.principalId}
        view={view} onView={setView} total={members.data?.totalCount ?? 0} pinFirst
        searchLabel="Search usernames or names" searchNote="Search, filters, and sorting apply to every member on the server."
        facets={[{ id: "role", label: "Role", options: ["owner", "admin", "member", "viewer"].map((r) => ({ value: r, label: roleLabel[r] })) },
          { id: "status", label: "Status", options: [{ value: "active", label: "Active" }, { value: "invited", label: "Invited" }, { value: "disabled", label: "Disabled" }] }]}
        canSelect={selectable} expandLabel={(m) => m.username}
        bulk={(rows, clear) => <>
          <button type="button" className="secondary-button danger-outline" disabled={!rows.some((m) => m.status !== "disabled")} onClick={() => setBulk({ kind: "disable", members: rows.filter((m) => m.status !== "disabled"), clear })}><UserX size={14} aria-hidden="true" /> Disable</button>
          <button type="button" className="secondary-button" disabled={!rows.some((m) => m.activeSessions > 0)} onClick={() => setBulk({ kind: "revoke", members: rows.filter((m) => m.activeSessions > 0), clear })}><LogOut size={14} aria-hidden="true" /> Sign out</button>
        </>}
        loading={members.isPending} refreshing={members.isFetching && !members.isPending}
        error={members.isError ? denied ? "Your session is not an organization owner or admin." : <>Members could not be loaded. <button type="button" className="text-action" onClick={() => void members.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState icon={<UsersIcon size={22} aria-hidden="true" />} title="No members yet" action={<Link className="primary-button" to="/admin/users/new"><Plus size={15} aria-hidden="true" /> Invite user</Link>} />} />
    </section>
    <p className="page-footnote">Changing a role, disabling, or resetting a password signs the member out everywhere. Only owners grant owner or change an owner, and an organization always keeps an owner.</p>
    {bulk ? <BulkMemberDialog kind={bulk.kind} members={bulk.members} onClose={(done) => { if (done) bulk.clear(); setBulk(null); }} /> : null}
  </PageShell>;
}
