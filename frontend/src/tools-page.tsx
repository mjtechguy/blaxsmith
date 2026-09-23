import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { createPaginatedRowModel, createSortedRowModel, rowPaginationFeature, rowSortingFeature, sortFns, tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ArrowUpDown, Plus, RefreshCw, Search } from "lucide-react";
import { PageHeader, PageShell } from "./page";
import { CatalogService, type ListToolsResponse, type ToolRelease } from "./gen/blaxsmith/api/v1/catalog_pb";

type Row = ToolRelease & { tool: string; package: string; isLatest: boolean; publisherStable?: string };
const catalogClient = createClient(CatalogService, createConnectTransport({ baseUrl: `${window.location.origin}/api` }));

async function fetchCatalog(signal: AbortSignal): Promise<ListToolsResponse> {
  return catalogClient.listTools({}, { signal });
}

const names: Record<string, string> = { codex: "Codex", "claude-code": "Claude Code", opencode: "OpenCode" };
const versionOrder = new Intl.Collator(undefined, { numeric: true });

const features = tableFeatures({ rowSortingFeature, rowPaginationFeature, sortedRowModel: createSortedRowModel(), paginatedRowModel: createPaginatedRowModel(), sortFns });

const columns: ColumnDef<typeof features, Row>[] = [
  { id: "tool", accessorKey: "tool", header: "Tool", cell: ({ row }) => <span className="tool-name">{names[row.original.tool] || row.original.tool}</span> },
  { id: "version", accessorKey: "version", header: "Version", sortFn: (a, b) => versionOrder.compare(a.original.version, b.original.version), cell: ({ row }) => <div className="version-cell"><code>{row.original.version}</code>{row.original.isLatest ? <span className="version-badge">Latest</span> : null}{row.original.publisherStable === row.original.version ? <span className="stable-badge">Publisher stable</span> : null}</div> },
  { id: "published_at", accessorKey: "publishedAt", header: "Released", cell: ({ row }) => <time dateTime={row.original.publishedAt}>{new Date(row.original.publishedAt).toLocaleDateString()}</time> },
  { id: "package", accessorKey: "package", header: "Package", cell: ({ row }) => <code className="package-name">{row.original.package}</code> },
  { id: "integrity", accessorKey: "integrity", header: "Integrity metadata", cell: ({ row }) => <code className="integrity" title={row.original.integrity}>{row.original.integrity.slice(0, 21)}…</code> },
];

export function ToolsPage() {
  const query = useQuery({ queryKey: ["public-runtime-catalog"], queryFn: ({ signal }) => fetchCatalog(signal) });
  const [tool, setTool] = useState("all");
  const [search, setSearch] = useState("");
  const rows = useMemo(() => (query.data?.tools || []).flatMap((entry) => entry.releases.map((release) => ({
    ...release, tool: entry.tool, package: entry.package, isLatest: release.version === entry.latestStable,
    publisherStable: entry.publisherStable,
  }))).filter((row) => (tool === "all" || row.tool === tool) &&
    `${row.tool} ${row.package} ${row.version}`.toLowerCase().includes(search.toLowerCase())), [query.data, tool, search]);
  const table = useTable({ features, data: rows, columns,
    getRowId: (row) => `${row.tool}@${row.version}`,
    initialState: { sorting: [{ id: "published_at", desc: true }], pagination: { pageIndex: 0, pageSize: 20 } }, autoResetPageIndex: true });

  return <PageShell>
    <PageHeader eyebrow="Platform" title="Tools & runtimes" description="Review recent publisher releases before an administrator validates and installs a pinned runtime." actions={<button type="button" className="primary-button" disabled title="Runtime installation is not available yet"><Plus size={16} aria-hidden="true" /> Add runtime</button>} />
    <div className="notice" role="note"><strong>Read-only catalog.</strong> These versions are discoverable, not approved or installed. Launch profiles will use exact validated image digests.</div>
    {query.data?.stale ? <div className="notice" role="status"><strong>Publisher unavailable.</strong> Showing the last successful catalog. Check the fetched time before making a runtime decision.</div> : null}
    {query.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading publisher releases</h2><p>Fetching exact version and integrity metadata.</p></div> : null}
    {query.isError ? <div className="state-panel" role="alert"><h2>Catalog unavailable</h2><p>{query.error.message}</p><button className="secondary-button" type="button" onClick={() => void query.refetch()}>Try again</button></div> : null}
    {query.data ? <>
      <div className="summary-grid">{query.data.tools.map((entry) => <section className="summary-card" key={entry.tool} aria-label={`${names[entry.tool]} release summary`}><span className="summary-label">{names[entry.tool]}</span><strong>{entry.latestStable}</strong><span className="summary-meta">{entry.publisherStable && entry.publisherStable !== entry.latestStable ? `Publisher stable ${entry.publisherStable}` : "Newest non-prerelease"}</span></section>)}</div>
      <section className="table-section" aria-labelledby="release-heading">
        <div className="table-heading"><div><h2 id="release-heading">Recent releases</h2><p>Showing up to 20 stable versions per tool. Version selection does not install a runtime.</p></div><span className="fetched-time">Fetched {new Date(query.data.tools[0]?.fetchedAt).toLocaleString()}</span></div>
        <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search releases</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search versions or packages" /></label><label className="filter-field"><span className="sr-only">Filter by tool</span><select value={tool} onChange={(event) => setTool(event.target.value)}><option value="all">All tools</option>{query.data.tools.map((entry) => <option key={entry.tool} value={entry.tool}>{names[entry.tool]}</option>)}</select></label></div>
        <div className="table-scroll"><table><thead>{table.getHeaderGroups().map((group) => <tr key={group.id}>{group.headers.map((header) => <th key={header.id} scope="col"><button type="button" onClick={header.column.getToggleSortingHandler()} aria-label={`Sort by ${String(header.column.columnDef.header)}`}><table.FlexRender header={header} />{header.column.getIsSorted() === "asc" ? <ArrowUp size={13} /> : header.column.getIsSorted() === "desc" ? <ArrowDown size={13} /> : <ArrowUpDown size={13} />}</button></th>)}</tr>)}</thead><tbody>{table.getRowModel().rows.map((row) => <tr key={row.id}>{row.getAllCells().map((cell) => <td key={cell.id}><table.FlexRender cell={cell} /></td>)}</tr>)}</tbody></table>{rows.length === 0 ? <div className="table-empty">No releases match this filter.</div> : null}</div>
        <div className="table-footer"><span>{rows.length} releases</span><div><button type="button" className="secondary-button" onClick={() => table.previousPage()} disabled={!table.getCanPreviousPage()}>Previous</button><span>Page {table.state.pagination.pageIndex + 1} of {Math.max(1, table.getPageCount())}</span><button type="button" className="secondary-button" onClick={() => table.nextPage()} disabled={!table.getCanNextPage()}>Next</button></div></div>
      </section>
    </> : null}
  </PageShell>;
}
