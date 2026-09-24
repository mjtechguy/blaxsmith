// Table view state in the URL (shareable: search, sort, page, filters) and
// per-user display preferences in browser storage (not shareable: visible
// columns, widths, density). Only display settings are stored locally.

export type SortEntry = { id: string; desc: boolean };
export type TableView = {
  q: string;
  sort: SortEntry[];
  page: number; // 1-based.
  size: number;
  filters: Record<string, string[]>;
};

export type ViewDefaults = { sort?: SortEntry[]; size?: number };
export const pageSizes = [10, 20, 50, 100] as const;
const DEFAULT_SIZE = 20;

const text = (value: unknown) => value === undefined || value === null ? "" : String(value);

// decodeView reads a view from route search params; unknown or malformed values fall back to defaults.
export function decodeView(search: Record<string, unknown>, defaults: ViewDefaults = {}): TableView {
  const sort = text(search.sort).split(",").filter(Boolean).map((part) => {
    const [id, direction] = part.split(".");
    return { id, desc: direction !== "asc" };
  }).filter((entry) => /^[\w-]{1,64}$/.test(entry.id));
  const page = Number.parseInt(text(search.page), 10);
  const size = Number.parseInt(text(search.size), 10);
  const filters: Record<string, string[]> = {};
  for (const [key, value] of Object.entries(search)) {
    if (!key.startsWith("f_")) continue;
    const values = text(value).split(",").map((v) => v.trim()).filter(Boolean);
    if (values.length) filters[key.slice(2)] = values;
  }
  return {
    q: text(search.q).slice(0, 160),
    sort: sort.length ? sort : defaults.sort ?? [],
    page: Number.isFinite(page) && page >= 1 ? page : 1,
    size: (pageSizes as readonly number[]).includes(size) ? size : defaults.size ?? DEFAULT_SIZE,
    filters,
  };
}

// encodeView writes the search params for a view, omitting defaults so URLs stay short.
// Keys owned by the table that are now empty are set to undefined so they drop out.
export function encodeView(view: TableView, previous: Record<string, unknown>, defaults: ViewDefaults = {}): Record<string, unknown> {
  const next: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(previous)) if (!key.startsWith("f_") && !["q", "sort", "page", "size"].includes(key)) next[key] = value;
  const sort = view.sort.map((s) => `${s.id}.${s.desc ? "desc" : "asc"}`).join(",");
  const defaultSort = (defaults.sort ?? []).map((s) => `${s.id}.${s.desc ? "desc" : "asc"}`).join(",");
  if (view.q) next.q = view.q;
  if (sort && sort !== defaultSort) next.sort = sort;
  if (view.page > 1) next.page = view.page;
  if (view.size !== (defaults.size ?? DEFAULT_SIZE)) next.size = view.size;
  for (const [id, values] of Object.entries(view.filters)) if (values.length) next[`f_${id}`] = values.join(",");
  return next;
}

// Changing what is shown returns to the first page.
export function withCriteria(view: TableView, change: Partial<Pick<TableView, "q" | "sort" | "filters" | "size">>): TableView {
  return { ...view, ...change, page: 1 };
}

export type Density = "comfortable" | "compact";
// hidden: columns the viewer turned off; shown: default-hidden columns they turned on.
export type TablePrefs = { hidden: string[]; shown: string[]; widths: Record<string, number>; density: Density };
export const defaultPrefs: TablePrefs = { hidden: [], shown: [], widths: {}, density: "comfortable" };

// A table may hide some columns by default (still offered in the Columns menu);
// the viewer's own choice, either way, wins and is remembered per user.
export function columnVisibility(prefs: TablePrefs, defaultHidden: string[] = []): Record<string, boolean> {
  return Object.fromEntries([
    ...defaultHidden.filter((id) => !prefs.shown.includes(id)).map((id) => [id, false] as const),
    ...prefs.hidden.map((id) => [id, false] as const),
  ]);
}

export function withVisibility(prefs: TablePrefs, next: Record<string, boolean>, defaultHidden: string[] = []): TablePrefs {
  const off = Object.entries(next).filter(([, shown]) => !shown).map(([id]) => id);
  return { ...prefs, hidden: off.filter((id) => !defaultHidden.includes(id)), shown: defaultHidden.filter((id) => next[id] === true) };
}

// Namespaced by product, version, user, and table so another account never sees these settings.
export const prefsKey = (principalId: string, tableId: string) => `blaxsmith:table:v1:${principalId || "anonymous"}:${tableId}`;

// fallbackDensity is the account-wide default from Account settings › Preferences.
export function readPrefs(principalId: string, tableId: string, fallbackDensity: Density = "comfortable"): TablePrefs {
  try {
    const raw = JSON.parse(globalThis.localStorage?.getItem(prefsKey(principalId, tableId)) || "null") as Partial<TablePrefs> | null;
    if (!raw || typeof raw !== "object") return { ...defaultPrefs, density: fallbackDensity };
    return {
      hidden: Array.isArray(raw.hidden) ? raw.hidden.filter((id): id is string => typeof id === "string") : [],
      shown: Array.isArray(raw.shown) ? raw.shown.filter((id): id is string => typeof id === "string") : [],
      widths: raw.widths && typeof raw.widths === "object" ? Object.fromEntries(Object.entries(raw.widths).filter(([, w]) => typeof w === "number" && w >= 40 && w <= 1200)) : {},
      density: raw.density === "compact" || raw.density === "comfortable" ? raw.density : fallbackDensity,
    };
  } catch {
    return { ...defaultPrefs, density: fallbackDensity };
  }
}

export function writePrefs(principalId: string, tableId: string, prefs: TablePrefs) {
  try { globalThis.localStorage?.setItem(prefsKey(principalId, tableId), JSON.stringify(prefs)); } catch { /* storage unavailable: preferences stay in memory */ }
}
