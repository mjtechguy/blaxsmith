import { useMemo, useState } from "react";
import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, useLocation } from "@tanstack/react-router";
import { AtSign, KeyRound, LogOut, UserCheck, UserCog, UserX } from "lucide-react";
import { personLabel } from "../account";
import { auditKey, listAuditEvents } from "../admin";
import { CollectionTable, useLocalView, type GridColumn } from "../data-table";
import type { AdminAuditEvent } from "../gen/blaxsmith/api/v1/admin_pb";
import { DetailLayout, SummaryList } from "../layouts";
import { Card, CopyValue, EmptyState, sentence, StatePanel, tabFrom, Timestamp, type TabSpec } from "../ui";
import { assignableRoles, canManage, listMembers, membersKey } from "../users";
import { MemberActionDialog, memberStatusBadge, roleLabel, type MemberAction } from "../users-ui";
import { useScope } from "../workspace-ui";

export const Route = createFileRoute("/admin/users/$principalId")({ component: UserDetail });

const tabs: TabSpec[] = [{ id: "profile", label: "Profile" }, { id: "sessions", label: "Sessions" }, { id: "activity", label: "Activity" }];

function UserDetail() {
  const { principalId } = Route.useParams();
  const { session, org } = useScope();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const tab = tabFrom(search, tabs);
  const members = useQuery({ queryKey: membersKey(org), enabled: Boolean(org), queryFn: ({ signal }) => listMembers(signal) });
  const member = members.data?.members.find((m) => m.principalId === principalId);
  const [action, setAction] = useState<MemberAction | null>(null);
  if (members.isPending) return <StatePanel kind="loading" title="Loading member" />;
  if (members.isError) return <StatePanel kind="error" title="Member unavailable" retry={() => void members.refetch()} />;
  if (!member) return <StatePanel kind="error" title="Member not found">They may have been removed from this organization.</StatePanel>;
  const self = member.principalId === session.data?.principalId;
  const manage = canManage(session.data, member.role);
  const why = manage ? undefined : "Only an owner can change an owner";
  const ask = (kind: MemberAction["kind"]) => setAction({ kind, member });

  return <DetailLayout back={{ href: "/admin/users", label: "Users" }} title={personLabel(member)}
    status={<><span className="state-badge">{roleLabel[member.role] ?? member.role}</span> <span className={`state-badge ${memberStatusBadge[member.status] ?? ""}`}>{sentence(member.status)}</span></>}
    facts={[
      { label: "Email", value: member.email || <span className="muted">Not set</span> },
      { label: "Last login", value: member.lastLoginAt ? <Timestamp value={member.lastLoginAt} /> : "Never" },
      { label: "Active sessions", value: member.activeSessions },
      { label: "Added", value: <Timestamp value={member.createdAt} /> },
    ]}
    actions={<>
      <button type="button" className="secondary-button" disabled={!manage} title={why} onClick={() => ask("role")}><UserCog size={15} aria-hidden="true" /> Change role</button>
      {member.status === "disabled"
        ? <button type="button" className="secondary-button" disabled={!manage} title={why} onClick={() => ask("enable")}><UserCheck size={15} aria-hidden="true" /> Enable</button>
        : <button type="button" className="secondary-button danger-outline" disabled={!manage || self} title={self ? "You cannot disable yourself" : why} onClick={() => ask("disable")}><UserX size={15} aria-hidden="true" /> Disable</button>}
    </>}
    tabs={tabs} current={tab} tabsLabel="User sections">
    {tab === "profile" ? <div className="dash-grid">
      <Card title="Profile" className="dash-main" description="Members sign in with their email and password. Names are set by the member.">
        <SummaryList items={[
          { label: "Display name", value: member.displayName || <span className="muted">Not set</span> },
          { label: "Email", value: member.email ? <>{member.email} <span className="state-badge">{member.emailVerified ? "Verified" : "Unverified"}</span></> : <span className="muted">Not set: they must add one at next sign-in</span> },
          { label: "Internal handle", value: <span className="mono">{member.username}</span> },
          { label: "Role", value: roleLabel[member.role] ?? member.role },
          { label: "Status", value: member.status === "invited" ? "Invited: setup link not used yet" : member.status },
          { label: "Principal ID", value: <CopyValue value={member.principalId} label="Principal ID" chars={13} /> },
        ]} />
      </Card>
      <Card title="Account access" className="dash-side" description="One-time links are copied by you; no email is sent.">
        <div className="card-body stack">
          <button type="button" className="secondary-button" disabled={!manage || member.status === "disabled"} title={member.status === "disabled" ? "Enable the member first" : why} onClick={() => ask("reset")}>
            <KeyRound size={15} aria-hidden="true" /> {member.status === "invited" ? "New setup link" : "Issue reset link"}</button>
          <button type="button" className="secondary-button" disabled={!manage} title={why} onClick={() => ask("email")}>
            <AtSign size={15} aria-hidden="true" /> Change email</button>
        </div>
      </Card>
    </div> : null}
    {tab === "sessions" ? <Card title="Sessions" description="The server records how many sessions are active; individual session details are not listed yet.">
      <div className="card-body stack">
        <p><strong>{member.activeSessions}</strong> active session{member.activeSessions === 1 ? "" : "s"}{member.lastLoginAt ? <>, last sign-in <Timestamp value={member.lastLoginAt} /></> : null}.</p>
        <button type="button" className="secondary-button danger-outline" disabled={!manage || member.activeSessions === 0} title={member.activeSessions === 0 ? "No active sessions" : why} onClick={() => ask("revoke")}>
          <LogOut size={15} aria-hidden="true" /> Sign out everywhere</button>
      </div>
    </Card> : null}
    {tab === "activity" ? <UserActivity principalId={principalId} /> : null}
    {action ? <MemberActionDialog action={action} roles={assignableRoles(session.data)} onClose={() => setAction(null)} /> : null}
  </DetailLayout>;
}

// Audit events this member performed, newest first (server filter by actor).
function UserActivity({ principalId }: { principalId: string }) {
  const { org } = useScope();
  const [view, setView] = useLocalView({ size: 20 });
  const events = useInfiniteQuery({
    queryKey: auditKey(org, "", principalId, ""), enabled: Boolean(org), initialPageParam: "", placeholderData: keepPreviousData,
    queryFn: ({ pageParam, signal }) => listAuditEvents(pageParam, "", principalId, "", signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => events.data?.pages.flatMap((p) => p.events) ?? [], [events.data]);
  const columns = useMemo<GridColumn<AdminAuditEvent>[]>(() => [
    { id: "action", accessorKey: "action", header: "Event", enableHiding: false, cell: ({ row }) => <span className="mono">{row.original.action}</span> },
    { id: "project", accessorKey: "projectName", header: "Project", cell: ({ row }) => row.original.projectName || "—" },
    { id: "subject", accessorKey: "subjectId", header: "Subject", enableSorting: false, cell: ({ row }) => <CopyValue value={row.original.subjectId} label="Subject ID" /> },
    { id: "when", accessorKey: "occurredAt", header: "When", cell: ({ row }) => <Timestamp value={row.original.occurredAt} /> },
  ], []);
  return <section className="table-section" aria-labelledby="user-activity-heading">
    <div className="table-heading"><div><h2 id="user-activity-heading">Activity</h2><p>Audit events this member performed.</p></div></div>
    <CollectionTable id="user-activity" label="Member activity" noun="events" columns={columns} data={rows} getRowId={(e) => e.id.toString()} view={view} onView={setView} paged={false} sortable={false}
      loading={events.isPending} error={events.isError ? "Activity could not be loaded." : undefined}
      empty={<EmptyState title="No recorded activity" />} />
    {events.hasNextPage ? <div className="table-footer"><span>{rows.length} loaded</span><button type="button" className="secondary-button" disabled={events.isFetchingNextPage} onClick={() => void events.fetchNextPage()}>{events.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
  </section>;
}
