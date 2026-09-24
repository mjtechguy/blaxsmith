import { useEffect, useMemo } from "react";
import { keepPreviousData, useInfiniteQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, FolderKanban, Plus } from "lucide-react";
import { CollectionTable, useUrlView, type GridColumn } from "../data-table";
import type { Project } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { CopyValue, EmptyState, Timestamp } from "../ui";
import { useScope } from "../workspace-ui";
import { listProjects, projectQueries } from "../workflow";

export const Route = createFileRoute("/projects/")({ component: Projects });

const defaults = { sort: [{ id: "created", desc: true }], size: 20 };
const columns: GridColumn<Project>[] = [
  { id: "name", accessorKey: "name", header: "Project", enableHiding: false, cell: ({ row }) =>
    <Link className="project-link" to="/projects/$projectId" params={{ projectId: row.original.id }}>
      <span className="project-symbol"><FolderKanban size={16} aria-hidden="true" /></span>
      <span><strong>{row.original.name}</strong><small>{row.original.slug}</small></span>
      <ArrowRight size={15} aria-hidden="true" />
    </Link> },
  { id: "id", accessorKey: "id", header: "Project ID", enableSorting: false, cell: ({ row }) => <CopyValue value={row.original.id} label="Project ID" /> },
  { id: "created", accessorKey: "createdAt", header: "Created", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
];

// The server pages projects by cursor with search and sort; the table fetches
// forward as it pages and shows "more" while another server page exists.
function Projects() {
  const { org, isMember } = useScope();
  const [view, setView] = useUrlView(defaults);
  const sortBy = view.sort[0]?.id === "name" ? "name" : "created_at";
  const direction = view.sort[0]?.desc === false ? "asc" : "desc";
  const projects = useInfiniteQuery({
    queryKey: projectQueries(org, view.q, sortBy, direction), enabled: Boolean(org), initialPageParam: "", placeholderData: keepPreviousData,
    queryFn: ({ pageParam, signal }) => listProjects(pageParam, view.q, sortBy, direction, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => projects.data?.pages.flatMap((page) => page.projects) ?? [], [projects.data]);
  const needed = view.page * view.size;
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = projects;
  useEffect(() => { if (hasNextPage && !isFetchingNextPage && rows.length < needed) void fetchNextPage(); }, [hasNextPage, isFetchingNextPage, fetchNextPage, rows.length, needed]);
  const total = hasNextPage ? Math.max(rows.length, needed) + 1 : rows.length;
  const pageRows = rows.slice((view.page - 1) * view.size, view.page * view.size);
  const create = isMember ? <Link className="primary-button" to="/projects/new"><Plus size={16} aria-hidden="true" /> New project</Link> : undefined;
  return <PageShell>
    <PageHeader title="Projects" description="Each project keeps a repository, its checks, recipes, connections, and runs together." actions={create} />
    <section className="table-section" aria-label="Projects">
      <CollectionTable id="projects" label="Projects" noun="projects" columns={columns} data={pageRows} getRowId={(p) => p.id}
        view={view} onView={setView} total={total} moreAvailable={Boolean(hasNextPage)} pinFirst searchLabel="Search projects" searchNote="Search and sorting run on the server across all projects."
        loading={projects.isPending || (isFetchingNextPage && !pageRows.length)} refreshing={projects.isFetching && !projects.isPending}
        error={projects.isError ? <>Projects could not be loaded. <button type="button" className="text-action" onClick={() => void projects.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState icon={<FolderKanban size={22} aria-hidden="true" />} title="No projects yet" action={create}>
          {isMember ? "Create a project to connect a repository and start runs." : "Ask an organization member to create the first project."}</EmptyState>} />
    </section>
  </PageShell>;
}
