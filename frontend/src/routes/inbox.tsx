import { useMemo } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useLocation, useNavigate } from "@tanstack/react-router";
import { ArrowRight } from "lucide-react";
import { CollectionTable, useUrlView } from "../data-table";
import { PageHeader, PageShell } from "../page";
import { EmptyState } from "../ui";
import { inboxColumns, useScope } from "../workspace-ui";
import { inboxKey, inboxKinds, listInbox } from "../workspace";

export const Route = createFileRoute("/inbox")({ component: InboxPage });

const defaults = { size: 25 };

function InboxPage() {
  const { scope, role } = useScope();
  const [view, setView] = useUrlView(defaults);
  const location = useLocation();
  const navigate = useNavigate();
  // "Mine" (actionable) is the default; ?all=1 shows everything open in the organization.
  const all = (location.search as Record<string, unknown>).all === 1 || (location.search as Record<string, unknown>).all === "1";
  const items = useQuery({ queryKey: inboxKey(scope, view, !all), enabled: Boolean(scope), placeholderData: keepPreviousData, refetchInterval: 15_000,
    queryFn: ({ signal }) => listInbox(view, !all, "", signal) });
  const columns = useMemo(() => inboxColumns(), []);
  const setAll = (value: boolean) => void navigate({ to: "/inbox", search: ((prev: Record<string, unknown>) => ({ ...prev, all: value ? 1 : undefined, page: undefined })) as never, replace: true });

  return <PageShell>
    <PageHeader title="Inbox" description="Questions, approvals, escalations, and final reviews across every project, plus model-gateway budget alerts. Answer on the run page, where the full context is." />
    <div className="segmented" role="group" aria-label="Which items">
      <button type="button" aria-pressed={!all} onClick={() => setAll(false)}>Waiting on you</button>
      <button type="button" aria-pressed={all} onClick={() => setAll(true)}>All open</button>
    </div>
    <section className="table-section" aria-labelledby="inbox-list-heading">
      <div className="table-heading"><div><h2 id="inbox-list-heading">{all ? "All open items" : "Waiting on you"}</h2>
        <p>{all ? "Everything open in the organization, including items for other roles." : role === "viewer" ? "Viewers cannot answer agents or decide reviews, so nothing is assigned to you." : role === "member" ? "Items you can answer. Final reviews are decided by owners and admins." : "Items you can answer or decide."}</p></div></div>
      <CollectionTable id="inbox" label="Inbox" noun="items" columns={columns} data={items.data?.items ?? []} getRowId={(row) => `${row.kind}:${row.id}`}
        view={view} onView={setView} total={items.data?.totalCount ?? 0} pinFirst sortable={false}
        searchLabel="Search titles, projects, runs, or stages" searchNote="Search and filters apply to every open item on the server."
        facets={[{ id: "kind", label: "Kind", options: inboxKinds }]}
        loading={items.isPending} refreshing={items.isFetching && !items.isPending}
        error={items.isError ? <>The inbox could not be loaded. <button type="button" className="text-action" onClick={() => void items.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState title={all ? "Nothing is open" : "Nothing is waiting on you"}
          action={all ? <Link className="text-action" to="/runs">See all runs <ArrowRight size={13} aria-hidden="true" /></Link> : <button type="button" className="text-action" onClick={() => setAll(true)}>Show all open items</button>}>
          Agents ask here when a stage needs a decision.</EmptyState>} />
    </section>
  </PageShell>;
}
