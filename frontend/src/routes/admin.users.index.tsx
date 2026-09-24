import { useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { createSortedRowModel, rowSortingFeature, sortFns, tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, KeyRound, LogOut, Plus, RefreshCw, Search, ShieldAlert, UserCheck, UserCog, UserX } from "lucide-react";
import { OneTimeLink } from "../account-link";
import { ago, isOrgAdmin } from "../admin";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { AccountLink, OrgMember } from "../gen/blaxsmith/api/v1/users_pb";
import { PageHeader, PageShell } from "../page";
import { assignableRoles, canManage, issueResetLink, listMembers, membersKey, revokeUserSessions, setUserEnabled, setUserRole, userAdminError } from "../users";

export const Route = createFileRoute("/admin/users/")({ component: Users });

type Pending =
  | { kind: "role"; member: OrgMember }
  | { kind: "disable" | "enable" | "reset" | "revoke"; member: OrgMember };

const features = tableFeatures({ rowSortingFeature, sortedRowModel: createSortedRowModel(), sortFns });
const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };
const statusBadge: Record<string, string> = { active: "state-succeeded", invited: "state-waiting", disabled: "state-failed" };
const titles: Record<Pending["kind"], string> = { role: "Change role", disable: "Disable member", enable: "Enable member", reset: "Issue reset link", revoke: "Revoke sessions" };

function Users() {
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const self = session.data?.principalId || "";
  const allowed = isOrgAdmin(session.data);
  const members = useQuery({ queryKey: membersKey(org), enabled: Boolean(org && allowed), queryFn: ({ signal }) => listMembers(signal) });
  const [search, setSearch] = useState("");
  const dialog = useRef<HTMLDialogElement>(null);
  const [pending, setPending] = useState<Pending | null>(null);
  const [role, setRole] = useState("member");
  const [issued, setIssued] = useState<AccountLink | null>(null);
  const [actionError, setActionError] = useState("");
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { setNow(Date.now()); }, [members.dataUpdatedAt]);

  const act = useMutation({
    mutationFn: async (action: Pending) => {
      const id = action.member.principalId;
      if (action.kind === "role") return setUserRole(id, role);
      if (action.kind === "disable" || action.kind === "enable") return setUserEnabled(id, action.kind === "enable");
      if (action.kind === "revoke") return revokeUserSessions(id);
      return issueResetLink(id);
    },
    onSuccess: async (response, action) => {
      if (action.kind === "reset" && "link" in response && response.link) setIssued(response.link);
      else setPending(null);
      await queryClient.invalidateQueries({ queryKey: membersKey(org) });
    },
    onError: (cause) => {
      setActionError(userAdminError(cause));
      void queryClient.invalidateQueries({ queryKey: membersKey(org) });
    },
  });
  const busy = act.isPending;
  const ask = (next: Pending) => {
    setActionError("");
    setIssued(null);
    setRole(next.member.role);
    setPending(next);
  };
  useEffect(() => {
    if (pending) dialog.current?.showModal();
    else if (dialog.current?.open) dialog.current.close();
  }, [pending]);

  const columns = useMemo<ColumnDef<typeof features, OrgMember>[]>(() => [
    { id: "username", accessorKey: "username", header: "Username", cell: ({ row }) => <span className="task-stage"><strong>{row.original.username}</strong>{row.original.principalId === self ? <small>You</small> : null}</span> },
    { id: "displayName", accessorKey: "displayName", header: "Display name", cell: ({ row }) => row.original.displayName || "—" },
    { id: "role", accessorKey: "role", header: "Role", sortFn: (a, b) => ["owner", "admin", "member", "viewer"].indexOf(a.original.role) - ["owner", "admin", "member", "viewer"].indexOf(b.original.role),
      cell: ({ row }) => <span className="state-badge">{roleLabel[row.original.role] ?? row.original.role}</span> },
    { id: "status", accessorKey: "status", header: "Status", cell: ({ row }) => <span className={`state-badge ${statusBadge[row.original.status] ?? ""}`}>{row.original.status}</span> },
    { id: "sessions", accessorKey: "activeSessions", header: "Sessions", cell: ({ row }) => row.original.activeSessions },
    { id: "lastLogin", accessorKey: "lastLoginAt", header: "Last login", cell: ({ row }) => row.original.lastLoginAt
      ? <time dateTime={row.original.lastLoginAt} title={new Date(row.original.lastLoginAt).toLocaleString()}>{ago(row.original.lastLoginAt, now)}</time> : "Never" },
    { id: "actions", header: "Actions", enableSorting: false, cell: ({ row }) => {
      const member = row.original;
      const manage = canManage(session.data, member.role);
      const reason = manage ? undefined : "Only an owner can change an owner";
      const label = member.username;
      return <span className="admin-actions">
        <button type="button" className="text-action" disabled={busy || !manage} title={reason} onClick={() => ask({ kind: "role", member })} aria-label={`Change role for ${label}`}><UserCog size={13} aria-hidden="true" /> Role</button>
        {member.status === "disabled"
          ? <button type="button" className="text-action" disabled={busy || !manage} title={reason} onClick={() => ask({ kind: "enable", member })} aria-label={`Enable ${label}`}><UserCheck size={13} aria-hidden="true" /> Enable</button>
          : <button type="button" className="text-action text-action-danger" disabled={busy || !manage || member.principalId === self} title={member.principalId === self ? "You cannot disable yourself" : reason} onClick={() => ask({ kind: "disable", member })} aria-label={`Disable ${label}`}><UserX size={13} aria-hidden="true" /> Disable</button>}
        <button type="button" className="text-action" disabled={busy || !manage || member.status === "disabled"} title={member.status === "disabled" ? "Enable the member first" : reason} onClick={() => ask({ kind: "reset", member })} aria-label={`Issue reset link for ${label}`}><KeyRound size={13} aria-hidden="true" /> {member.status === "invited" ? "New setup link" : "Reset link"}</button>
        <button type="button" className="text-action text-action-danger" disabled={busy || !manage || member.activeSessions === 0} title={member.activeSessions === 0 ? "No active sessions" : reason} onClick={() => ask({ kind: "revoke", member })} aria-label={`Revoke sessions for ${label}`}><LogOut size={13} aria-hidden="true" /> Sign out</button>
      </span>;
    } },
  ], [busy, now, self, session.data]);

  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase();
    return (members.data?.members || []).filter((m) => !needle || `${m.username} ${m.displayName}`.toLowerCase().includes(needle));
  }, [members.data, search]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.principalId, initialState: { sorting: [{ id: "role", desc: false }] } });

  if (session.data && !allowed) return <PageShell><div className="state-panel" role="alert"><ShieldAlert size={22} aria-hidden="true" /><h2>Administration is restricted</h2><p>Only organization owners and admins can manage members.</p><Link className="secondary-button" to="/">Back to workspace</Link></div></PageShell>;
  const denied = members.isError && ConnectError.from(members.error).code === Code.PermissionDenied;
  const roles = assignableRoles(session.data);

  return <PageShell>
    <PageHeader eyebrow="Administration" title="Users" description="Organization members, their roles, and account access. Invitations and resets use one-time links you copy; no email is sent."
      actions={<Link className="primary-button" to="/admin/users/new"><Plus size={15} aria-hidden="true" /> Invite user</Link>} />
    <Link to="/admin" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Operations</Link>
    <section className="table-section" aria-labelledby="members-heading">
      <div className="table-heading"><div><h2 id="members-heading">Members</h2><p>Changing a role, disabling, or resetting a password signs the member out everywhere. Only owners grant owner or change an owner, and an organization always keeps an owner.</p></div>
        <button type="button" className="secondary-button" disabled={members.isFetching} onClick={() => void members.refetch()}><RefreshCw size={14} aria-hidden="true" className={members.isFetching ? "spin" : undefined} /> Refresh</button></div>
      <div className="table-toolbar">
        <label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search members</span>
          <input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Username or display name" maxLength={160} /></label>
      </div>
      {members.isPending ? <div className="source-summary" role="status">Loading members…</div> : null}
      {members.isError ? <div className="source-summary" role="alert">{denied ? "Your session is not an organization owner or admin." : "Members could not be loaded."} {denied ? null : <button type="button" className="text-action" onClick={() => void members.refetch()}>Try again</button>}</div> : null}
      <DataTable table={table} label="Members" empty={members.isPending || members.isError ? undefined : search ? "No members match this search." : "No members yet."} />
      <div className="table-footer"><span>{rows.length} of {members.data?.members.length ?? 0} members</span></div>
    </section>

    <dialog ref={dialog} className="review-confirm" aria-labelledby="users-confirm-title"
      onCancel={(event) => { if (busy) event.preventDefault(); }}
      onClose={() => { if (!busy) { setPending(null); setIssued(null); setActionError(""); } }}>
      {pending ? <>
        <h3 id="users-confirm-title">{issued ? (issued.purpose === "reset" ? "Reset link ready" : "Setup link ready") : titles[pending.kind]}</h3>
        {issued ? <OneTimeLink link={issued} username={pending.member.username} /> : <>
          {pending.kind === "role" ? <>
            <p>Choose a new role for <strong>{pending.member.username}</strong>. Their current sessions end so the new role applies immediately.</p>
            <div className="form-field"><label htmlFor="users-role">Role</label>
              <select id="users-role" value={role} onChange={(event) => setRole(event.target.value)}>
                {roles.map((entry) => <option key={entry} value={entry}>{roleLabel[entry]}</option>)}</select></div>
          </> : null}
          {pending.kind === "disable" ? <p>Disable <strong>{pending.member.username}</strong>? They are signed out now, cannot sign in, and any open setup or reset link stops working. You can enable them again later.</p> : null}
          {pending.kind === "enable" ? <p>Enable <strong>{pending.member.username}</strong>? They can sign in again with their existing password and keep the role <strong>{roleLabel[pending.member.role]}</strong>.</p> : null}
          {pending.kind === "reset" ? <p>Issue a one-time {pending.member.status === "invited" ? "setup" : "password reset"} link for <strong>{pending.member.username}</strong>? Any earlier open link stops working. Using it sets a new password and signs them out everywhere.</p> : null}
          {pending.kind === "revoke" ? <p>Sign <strong>{pending.member.username}</strong> out of all {pending.member.activeSessions} active session{pending.member.activeSessions === 1 ? "" : "s"}? Their password is unchanged.</p> : null}
          <p className="admin-note">This change is recorded in the audit log.</p>
        </>}
        {actionError ? <p className="auth-alert" role="alert">{actionError}</p> : null}
        <div className="review-confirm-actions">
          {issued ? <button type="button" className="primary-button" onClick={() => setPending(null)}>Done</button> : <>
            <button type="button" className="secondary-button" disabled={busy} onClick={() => setPending(null)}>Cancel</button>
            <button type="button" className={`primary-button${pending.kind === "disable" || pending.kind === "revoke" ? " danger-button" : ""}`}
              disabled={busy || (pending.kind === "role" && role === pending.member.role)} onClick={() => act.mutate(pending)}>
              {busy ? "Working…" : titles[pending.kind]}</button>
          </>}
        </div>
      </> : null}
    </dialog>
  </PageShell>;
}
