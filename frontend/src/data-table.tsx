import { Fragment, useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { useLocation, useNavigate } from "@tanstack/react-router";
import {
  columnFacetingFeature, columnFilteringFeature, columnPinningFeature, columnResizingFeature, columnSizingFeature, columnVisibilityFeature,
  createExpandedRowModel, createFacetedRowModel, createFacetedUniqueValues, createFilteredRowModel, createPaginatedRowModel, createSortedRowModel,
  globalFilteringFeature, rowExpandingFeature, rowPaginationFeature, rowSelectionFeature, rowSortingFeature, sortFns, tableFeatures, useTable,
  type Column, type ColumnDef, type Header, type ReactTable, type Row, type RowData, type RowSelectionState, type TableFeatures,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ArrowUpDown, ChevronDown, ChevronLeft, ChevronRight, Columns3, Rows3, Search, X } from "lucide-react";
import { currentSession, sessionQueryKey } from "./auth";
import { usePrefs } from "./preferences";
import { columnVisibility, decodeView, encodeView, pageSizes, readPrefs, withCriteria, withVisibility, writePrefs, type TablePrefs, type TableView, type ViewDefaults } from "./table-state";

type Props<TFeatures extends TableFeatures, TData extends RowData> = {
  table: ReactTable<TFeatures, TData>;
  label: string;
  empty?: string;
  header?: (header: Header<TFeatures, TData>) => ReactNode;
};

// Compact renderer for small, bounded detail matrices (stage lists, grants on
// a detail tab). Collections use CollectionTable below.
export function DataTable<TFeatures extends TableFeatures, TData extends RowData>({ table, label, empty, header: renderHeader }: Props<TFeatures, TData>) {
  return <div className="table-scroll" role="region" aria-label={`${label} table`} tabIndex={0}>
    <table aria-label={label}>
      <thead>{table.getHeaderGroups().map((group) => <tr key={group.id}>{group.headers.map((header) => {
        const column = header.column;
        const sorted = "getCanSort" in column && column.getCanSort() && "getIsSorted" in column ? column.getIsSorted() : null;
        return <th key={header.id} scope="col" aria-sort={sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : sorted === false ? "none" : undefined}>
          {renderHeader ? renderHeader(header) : sorted !== null ? <button type="button" onClick={() => { if ("toggleSorting" in column) column.toggleSorting(); }} aria-label={`Sort by ${String(column.columnDef.header)}`}>
            <table.FlexRender header={header} />{sorted === "asc" ? <ArrowUp size={13} /> : sorted === "desc" ? <ArrowDown size={13} /> : <ArrowUpDown size={13} />}
          </button> : <span className="table-label"><table.FlexRender header={header} /></span>}
        </th>;
      })}</tr>)}</thead>
      <tbody>{table.getRowModel().rows.map((row) => <tr key={row.id}>{row.getAllCells().map((cell) => <td key={cell.id}><table.FlexRender cell={cell} /></td>)}</tr>)}</tbody>
    </table>
    {empty && table.getRowModel().rows.length === 0 ? <div className="table-empty">{empty}</div> : null}
  </div>;
}

// One feature set for every collection: sorting, search, faceted filters,
// visibility, resizing, a pinned first column, selection, expansion, paging.
export const gridFeatures = tableFeatures({
  rowSortingFeature, sortedRowModel: createSortedRowModel(), sortFns,
  columnFilteringFeature, filteredRowModel: createFilteredRowModel(), globalFilteringFeature,
  columnFacetingFeature, facetedRowModel: createFacetedRowModel(), facetedUniqueValues: createFacetedUniqueValues(),
  columnVisibilityFeature, columnSizingFeature, columnResizingFeature, columnPinningFeature, // sizing state holds widths
  rowSelectionFeature, rowExpandingFeature, expandedRowModel: createExpandedRowModel(),
  rowPaginationFeature, paginatedRowModel: createPaginatedRowModel(),
});
export type GridFeatures = typeof gridFeatures;
export type GridColumn<T extends RowData> = ColumnDef<GridFeatures, T>;
type GridRow<T extends RowData> = Row<GridFeatures, T>;
type GridColumnApi<T extends RowData> = Column<GridFeatures, T>;

// Facet filter: the row's value (or any of its values) is one of the chosen ones.
export function inSet<T extends RowData>(row: GridRow<T>, columnId: string, chosen: unknown): boolean {
  const values = Array.isArray(chosen) ? chosen.map(String) : [];
  if (!values.length) return true;
  const value = row.getValue(columnId);
  return Array.isArray(value) ? value.some((v) => values.includes(String(v))) : values.includes(String(value ?? ""));
}

export type Facet = { id: string; label: string; options: Array<{ value: string; label: string }> };

// URL-backed view: search, sort, page, size, and filters live in the route search.
export function useUrlView(defaults: ViewDefaults = {}): [TableView, (view: TableView) => void] {
  const location = useLocation();
  const navigate = useNavigate();
  const search = location.search as Record<string, unknown>;
  const key = JSON.stringify(search);
  const defaultsKey = JSON.stringify(defaults);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const view = useMemo(() => decodeView(search, defaults), [key, defaultsKey]);
  const setView = useCallback((next: TableView) => {
    void navigate({ to: location.pathname as never, search: encodeView(next, search, defaults) as never, replace: true, resetScroll: false });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [navigate, location.pathname, key, defaultsKey]);
  return [view, setView];
}

// In-memory view for secondary tables that do not own the URL.
export function useLocalView(defaults: ViewDefaults = {}): [TableView, (view: TableView) => void] {
  const [view, setView] = useState(() => decodeView({}, defaults));
  return [view, setView];
}

function useTablePrefs(tableId: string): [TablePrefs, (next: TablePrefs) => void] {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const principal = session.data?.principalId || "";
  const { density } = usePrefs();
  const [prefs, setPrefs] = useState<TablePrefs>(() => readPrefs(principal, tableId, density));
  useEffect(() => { setPrefs(readPrefs(principal, tableId, density)); }, [principal, tableId, density]);
  return [prefs, (next) => { setPrefs(next); writePrefs(principal, tableId, next); }];
}

type Resolve<T> = T | ((old: T) => T);
const resolve = <T,>(updater: Resolve<T>, old: T): T => typeof updater === "function" ? (updater as (o: T) => T)(old) : updater;

export type CollectionProps<T extends RowData> = {
  id: string; // Stable table id for preferences.
  label: string;
  columns: GridColumn<T>[];
  data: T[];
  getRowId: (row: T) => string;
  view: TableView;
  onView: (view: TableView) => void;
  total?: number; // Set for server-paged collections: data is the current page.
  facets?: Facet[];
  searchLabel?: string; // Omit to hide search.
  searchNote?: string; // Scope of the search, e.g. "Searches every run in the organization".
  pinFirst?: boolean;
  defaultHidden?: string[]; // Column ids hidden until the viewer turns them on in Columns.
  canSelect?: (row: T) => boolean;
  bulk?: (rows: T[], clear: () => void) => ReactNode;
  renderExpanded?: (row: T) => ReactNode;
  canExpand?: (row: T) => boolean;
  expandLabel?: (row: T) => string;
  loading?: boolean;
  refreshing?: boolean;
  error?: ReactNode;
  empty: ReactNode;
  noun?: string; // Plural noun for counts, e.g. "runs".
  toolbar?: ReactNode;
  paged?: boolean; // Client collections page by default; false shows every row.
  sortable?: boolean; // False when the server fixes the order.
  moreAvailable?: boolean; // Cursor-paged server: the total is unknown, another page exists.
};

const UTIL = 36;

export function CollectionTable<T extends RowData>(props: CollectionProps<T>) {
  const { id, label, columns: dataColumns, data, getRowId, view, onView, total, facets = [], searchLabel, searchNote, pinFirst, defaultHidden, canSelect, bulk,
    renderExpanded, canExpand, expandLabel, loading, refreshing, error, empty, noun = "rows", toolbar, paged = true, sortable = true, moreAvailable = false } = props;
  const server = total !== undefined;
  const [prefs, setPrefs] = useTablePrefs(id);
  const [rowSelection, setRowSelection] = useState<RowSelectionState>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const viewKey = JSON.stringify([view.q, view.sort, view.filters, view.page, view.size]);
  useEffect(() => { setRowSelection({}); }, [viewKey]);

  const columns = useMemo<GridColumn<T>[]>(() => {
    const util: GridColumn<T>[] = [];
    if (canSelect) util.push({ id: "_select", header: "Select", enableSorting: false, enableHiding: false, enableResizing: false, enableGlobalFilter: false, size: UTIL,
      cell: ({ row }) => <input type="checkbox" className="grid-check" checked={row.getIsSelected()} disabled={!row.getCanSelect()} onChange={row.getToggleSelectedHandler()}
        aria-label={`Select ${expandLabel ? expandLabel(row.original) : row.id}`} /> });
    if (renderExpanded) util.push({ id: "_expand", header: "Details", enableSorting: false, enableHiding: false, enableResizing: false, enableGlobalFilter: false, size: UTIL,
      cell: ({ row }) => row.getCanExpand() ? <button type="button" className="grid-expander" aria-expanded={row.getIsExpanded()} onClick={row.getToggleExpandedHandler()}
        aria-label={`${row.getIsExpanded() ? "Hide" : "Show"} details for ${expandLabel ? expandLabel(row.original) : row.id}`}><ChevronRight size={14} aria-hidden="true" /></button> : null });
    return [...util, ...dataColumns];
  }, [dataColumns, canSelect, renderExpanded, expandLabel]);

  const leading = columns.slice(0, (canSelect ? 1 : 0) + (renderExpanded ? 1 : 0) + (pinFirst ? 1 : 0)).map((c) => c.id as string);
  const pinned = pinFirst ? leading : leading.filter((c) => c.startsWith("_"));
  const visibility = columnVisibility(prefs, defaultHidden);
  const pageCount = server ? Math.max(1, Math.ceil((total ?? 0) / view.size)) : undefined;

  const table = useTable({
    features: gridFeatures, data, columns, getRowId: (row) => getRowId(row),
    state: {
      sorting: view.sort, globalFilter: server ? "" : view.q,
      columnFilters: server ? [] : Object.entries(view.filters).map(([fid, value]) => ({ id: fid, value })),
      pagination: { pageIndex: view.page - 1, pageSize: paged ? view.size : Math.max(data.length, 1) },
      columnVisibility: visibility, columnSizing: prefs.widths, columnPinning: { start: pinned, end: [] },
      rowSelection, expanded,
    },
    manualSorting: server, manualFiltering: server, manualPagination: server, rowCount: server ? total : undefined, pageCount,
    autoResetPageIndex: false, enableSorting: sortable, enableSortingRemoval: false, enableMultiSort: false, columnResizeMode: "onChange",
    enableRowSelection: canSelect ? (row) => canSelect(row.original) : false,
    getRowCanExpand: renderExpanded ? (row) => canExpand ? canExpand(row.original) : true : undefined,
    globalFilterFn: (row, columnId, value) => String(row.getValue(columnId) ?? "").toLowerCase().includes(String(value).toLowerCase()),
    onSortingChange: (u) => onView(withCriteria(view, { sort: resolve(u, view.sort) })),
    onPaginationChange: (u) => {
      const next = resolve(u, { pageIndex: view.page - 1, pageSize: view.size });
      onView(next.pageSize !== view.size ? withCriteria(view, { size: next.pageSize }) : { ...view, page: next.pageIndex + 1 });
    },
    onColumnVisibilityChange: (u) => {
      const next = resolve(u, visibility);
      setPrefs(withVisibility(prefs, next, defaultHidden));
    },
    onColumnSizingChange: (u) => setPrefs({ ...prefs, widths: resolve(u, prefs.widths) }),
    onRowSelectionChange: (u) => setRowSelection((old) => resolve(u, old)),
    onExpandedChange: (u) => setExpanded((old) => { const next = resolve(u as Resolve<Record<string, boolean>>, old); return next === true as unknown ? old : next; }),
  });

  // Reconcile an out-of-range page (e.g. after filters or a bulk action shrink the set).
  const lastPage = server ? pageCount! : Math.max(1, table.getPageCount());
  useEffect(() => { if (!loading && view.page > lastPage) onView({ ...view, page: lastPage }); }, [loading, view, lastPage, onView]);

  const rows = table.getRowModel().rows;
  const selected = table.getSelectedRowModel().rows.map((r) => r.original);
  const filteredCount = server ? total! : table.getFilteredRowModel().rows.length;
  const criteria = Boolean(view.q) || Object.values(view.filters).some((v) => v.length);
  const visibleColumns = table.getVisibleLeafColumns();
  const hideable = table.getAllLeafColumns().filter((c) => c.getCanHide());
  const clearCriteria = () => onView(withCriteria(view, { q: "", filters: {} }));
  const first = filteredCount === 0 ? 0 : (view.page - 1) * (paged ? view.size : filteredCount) + 1;
  const lastShown = Math.min(filteredCount, first + rows.length - 1);

  return <div className={`grid density-${prefs.density}${pinFirst ? " has-pinned" : ""}`}>
    <div className="grid-toolbar">
      {searchLabel ? <SearchBox label={searchLabel} value={view.q} onChange={(q) => onView(withCriteria(view, { q }))} /> : null}
      {facets.map((facet) => <FacetFilter key={facet.id} facet={facet} chosen={view.filters[facet.id] ?? []}
        counts={server ? undefined : table.getColumn(facet.id)?.getFacetedUniqueValues()}
        onChange={(values) => onView(withCriteria(view, { filters: { ...view.filters, [facet.id]: values } }))} />)}
      <div className="grid-toolbar-end">
        {toolbar}
        {refreshing ? <span className="fetched-time" role="status">Refreshing…</span> : null}
        <button type="button" className="icon-button grid-tool" aria-pressed={prefs.density === "compact"} title="Compact rows"
          aria-label="Compact rows" onClick={() => setPrefs({ ...prefs, density: prefs.density === "compact" ? "comfortable" : "compact" })}><Rows3 size={16} aria-hidden="true" /></button>
        {hideable.length > 1 ? <Menu label="Columns" icon={<Columns3 size={16} aria-hidden="true" />}>
          {hideable.map((column) => <label key={column.id} className="menu-check"><input type="checkbox" checked={column.getIsVisible()}
            disabled={column.getIsVisible() && hideable.filter((c) => c.getIsVisible()).length === 1} onChange={column.getToggleVisibilityHandler()} /> {headerText(column)}</label>)}
          {prefs.hidden.length || prefs.shown.length || Object.keys(prefs.widths).length ? <button type="button" className="text-action" onClick={() => setPrefs({ ...prefs, hidden: [], shown: [], widths: {} })}>Reset columns</button> : null}
        </Menu> : null}
      </div>
    </div>
    {searchNote && searchLabel ? <p className="grid-note">{searchNote}</p> : null}
    {criteria ? <div className="grid-chips" aria-label="Active filters">
      {view.q ? <span className="chip">Search: “{view.q}”<button type="button" aria-label="Clear search" onClick={() => onView(withCriteria(view, { q: "" }))}><X size={12} aria-hidden="true" /></button></span> : null}
      {Object.entries(view.filters).flatMap(([fid, values]) => values.map((value) => {
        const facet = facets.find((f) => f.id === fid);
        const text = `${facet?.label ?? fid}: ${facet?.options.find((o) => o.value === value)?.label ?? value}`;
        return <span className="chip" key={`${fid}:${value}`}>{text}<button type="button" aria-label={`Remove filter ${text}`}
          onClick={() => onView(withCriteria(view, { filters: { ...view.filters, [fid]: values.filter((v) => v !== value) } }))}><X size={12} aria-hidden="true" /></button></span>;
      }))}
      <button type="button" className="text-action" onClick={clearCriteria}>Clear all</button>
    </div> : null}
    {bulk && selected.length ? <div className="grid-bulk" role="region" aria-label="Bulk actions">
      <strong>{selected.length} selected</strong><span className="grid-bulk-scope">on this page</span>
      {bulk(selected, () => setRowSelection({}))}
      <button type="button" className="text-action" onClick={() => setRowSelection({})}>Clear selection</button>
    </div> : null}
    {error ? <div className="grid-state" role="alert">{error}</div> : null}
    <div className="table-scroll grid-scroll" role="region" aria-label={`${label} table`} tabIndex={0}>
      <table aria-label={label} aria-busy={loading || undefined} aria-rowcount={server ? total : undefined}>
        <colgroup>{visibleColumns.map((column) => <col key={column.id} width={column.id.startsWith("_") ? UTIL : prefs.widths[column.id]} />)}</colgroup>
        <thead>{table.getHeaderGroups().map((group) => <tr key={group.id}>{group.headers.map((header) => {
          const column = header.column;
          const sortable = column.getCanSort();
          const sorted = column.getIsSorted();
          const pin = pinned.indexOf(column.id);
          const text = headerText(column);
          return <th key={header.id} scope="col" className={cellClass(column.id, pin, pinned.length)} aria-sort={sortable ? sorted === "asc" ? "ascending" : sorted === "desc" ? "descending" : "none" : undefined}>
            {column.id === "_select" ? <input type="checkbox" className="grid-check" aria-label="Select all rows on this page"
              checked={table.getIsAllPageRowsSelected()} ref={(el) => { if (el) el.indeterminate = table.getIsSomePageRowsSelected(); }}
              onChange={table.getToggleAllPageRowsSelectedHandler()} />
              : column.id === "_expand" ? <span className="sr-only">Details</span>
                : sortable ? <button type="button" onClick={column.getToggleSortingHandler()} aria-label={`Sort by ${text}`}>
                  <table.FlexRender header={header} />{sorted === "asc" ? <ArrowUp size={13} aria-hidden="true" /> : sorted === "desc" ? <ArrowDown size={13} aria-hidden="true" /> : <ArrowUpDown size={13} aria-hidden="true" />}
                </button> : <span className="table-label"><table.FlexRender header={header} /></span>}
            {column.getCanResize() && !column.id.startsWith("_") ? <ResizeHandle text={text}
              onWidth={(width) => setPrefs({ ...prefs, widths: { ...prefs.widths, [column.id]: Math.round(Math.min(1200, Math.max(60, width))) } })}
              onReset={() => { const { [column.id]: _dropped, ...widths } = prefs.widths; setPrefs({ ...prefs, widths }); }} /> : null}
          </th>;
        })}</tr>)}</thead>
        <tbody>{rows.map((row) => <Fragment key={row.id}>
          <tr className={`${row.getIsSelected() ? "is-selected" : ""}${row.getIsExpanded() ? " is-expanded" : ""}`}>
            {row.getVisibleCells().map((cell) => <td key={cell.id} className={cellClass(cell.column.id, pinned.indexOf(cell.column.id), pinned.length)}><table.FlexRender cell={cell} /></td>)}
          </tr>
          {row.getIsExpanded() && renderExpanded ? <tr className="grid-detail"><td colSpan={visibleColumns.length}>{renderExpanded(row.original)}</td></tr> : null}
        </Fragment>)}</tbody>
      </table>
      {loading && !rows.length ? <div className="table-empty" role="status">Loading {noun}…</div> : null}
      {!loading && !error && !rows.length ? criteria || (server && view.page > 1)
        ? <div className="table-empty">No {noun} match these filters. <button type="button" className="text-action" onClick={clearCriteria}>Clear filters</button></div>
        : <div className="grid-empty">{empty}</div> : null}
    </div>
    {paged && (filteredCount > 0 || view.page > 1) ? <div className="table-footer grid-footer">
      <span>{first}–{lastShown} {moreAvailable ? `of more than ${lastShown}` : `of ${filteredCount}`} {noun}</span>
      <div>
        <label className="grid-size">Rows per page <select value={view.size} onChange={(event) => table.setPageSize(Number(event.target.value))}>
          {pageSizes.map((size) => <option key={size} value={size}>{size}</option>)}</select></label>
        <button type="button" className="secondary-button" disabled={view.page <= 1} onClick={() => onView({ ...view, page: view.page - 1 })} aria-label="Previous page"><ChevronLeft size={14} aria-hidden="true" /></button>
        <span aria-live="polite">Page {Math.min(view.page, lastPage)} of {lastPage}</span>
        <button type="button" className="secondary-button" disabled={view.page >= lastPage} onClick={() => onView({ ...view, page: view.page + 1 })} aria-label="Next page"><ChevronRight size={14} aria-hidden="true" /></button>
      </div>
    </div> : null}
  </div>;
}

function cellClass(columnId: string, pin: number, pinnedCount: number) {
  const parts: string[] = [];
  if (columnId.startsWith("_")) parts.push("grid-util");
  if (pin >= 0) parts.push("is-pinned", `pin-${pin}`);
  if (pin >= 0 && pin === pinnedCount - 1) parts.push("pin-edge");
  return parts.join(" ") || undefined;
}

function headerText<T extends RowData>(column: GridColumnApi<T>): string {
  const header = column.columnDef.header;
  return typeof header === "string" ? header : column.id;
}

// Drag (mouse or touch) or arrow keys resize a column; double-click restores auto width.
function ResizeHandle({ text, onWidth, onReset }: { text: string; onWidth: (width: number) => void; onReset: () => void }) {
  const [dragging, setDragging] = useState(false);
  const width = (el: Element) => el.closest("th")?.getBoundingClientRect().width ?? 150;
  const start = (startX: number, el: Element, touch: boolean) => {
    const startWidth = width(el);
    setDragging(true);
    const move = (event: MouseEvent | TouchEvent) => {
      const x = "touches" in event ? event.touches[0]?.clientX ?? startX : event.clientX;
      onWidth(startWidth + x - startX);
    };
    const end = () => {
      setDragging(false);
      document.removeEventListener(touch ? "touchmove" : "mousemove", move);
      document.removeEventListener(touch ? "touchend" : "mouseup", end);
    };
    document.addEventListener(touch ? "touchmove" : "mousemove", move);
    document.addEventListener(touch ? "touchend" : "mouseup", end);
  };
  return <span role="separator" aria-orientation="vertical" aria-label={`Resize ${text} column`} tabIndex={0}
    className={`grid-resizer${dragging ? " is-resizing" : ""}`}
    onMouseDown={(event) => { event.preventDefault(); start(event.clientX, event.currentTarget, false); }}
    onTouchStart={(event) => start(event.touches[0]?.clientX ?? 0, event.currentTarget, true)}
    onDoubleClick={onReset}
    onKeyDown={(event: KeyboardEvent) => {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      event.preventDefault();
      onWidth(width(event.currentTarget) + (event.key === "ArrowLeft" ? -16 : 16));
    }} />;
}

function SearchBox({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  const [draft, setDraft] = useState(value);
  const committed = useRef(value);
  useEffect(() => { if (value !== committed.current) { committed.current = value; setDraft(value); } }, [value]);
  useEffect(() => {
    if (draft.trim() === committed.current) return;
    const timer = window.setTimeout(() => { committed.current = draft.trim(); onChange(draft.trim()); }, 250);
    return () => window.clearTimeout(timer);
  }, [draft, onChange]);
  return <label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">{label}</span>
    <input type="search" value={draft} onChange={(event) => setDraft(event.target.value)} placeholder={label} maxLength={120} /></label>;
}

// A small disclosure menu (not a drawer): button + panel below it, closed by Escape or an outside click.
export function Menu({ label, icon, children, align = "end" }: { label: string; icon?: ReactNode; children: ReactNode; align?: "start" | "end" }) {
  const [isOpen, setOpen] = useState(false);
  const panelId = useId();
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!isOpen) return;
    const close = (event: MouseEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false); };
    const key = (event: globalThis.KeyboardEvent) => { if (event.key === "Escape") { setOpen(false); button.current?.focus(); } };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => { document.removeEventListener("mousedown", close); document.removeEventListener("keydown", key); };
  }, [isOpen]);
  return <div className="menu" ref={root}>
    <button ref={button} type="button" className="secondary-button menu-button-toggle" aria-expanded={isOpen} aria-controls={panelId} onClick={() => setOpen(!isOpen)}>
      {icon}<span>{label}</span><ChevronDown size={13} aria-hidden="true" /></button>
    {isOpen ? <div id={panelId} className={`menu-panel menu-${align}`} role="group" aria-label={label}>{children}</div> : null}
  </div>;
}

function FacetFilter({ facet, chosen, counts, onChange }: { facet: Facet; chosen: string[]; counts?: Map<unknown, number>; onChange: (values: string[]) => void }) {
  return <Menu align="start" label={chosen.length ? `${facet.label} · ${chosen.length}` : facet.label}>
    <fieldset className="menu-fieldset"><legend className="sr-only">Filter by {facet.label}</legend>
      {facet.options.map((option) => <label key={option.value} className="menu-check">
        <input type="checkbox" checked={chosen.includes(option.value)} onChange={(event) => onChange(event.target.checked ? [...chosen, option.value] : chosen.filter((v) => v !== option.value))} />
        <span>{option.label}</span>{counts ? <span className="menu-count">{counts.get(option.value) ?? 0}</span> : null}
      </label>)}
    </fieldset>
    {chosen.length ? <button type="button" className="text-action" onClick={() => onChange([])}>Clear {facet.label.toLowerCase()}</button> : null}
  </Menu>;
}
