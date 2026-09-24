import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { useLocation, useNavigate } from "@tanstack/react-router";
import { Search } from "lucide-react";
import { getAdminOverview } from "./admin";
import { listConnections } from "./connections";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";
import { addRecent, filterPalette, groupOrder, paletteItems, type PaletteItem } from "./palette";
import { listRecipes } from "./recipes";
import { listMembers } from "./users";
import { getProject, listProjects, listRuns } from "./workflow";

const isAdmin = (role: string) => role === "owner" || role === "admin";

function readRecents(key: string): PaletteItem[] {
  try { const parsed = JSON.parse(localStorage.getItem(key) || "[]"); return Array.isArray(parsed) ? parsed : []; } catch { return []; }
}

// ⌘K / Ctrl+K opens the palette anywhere in the signed-in shell.
function usePaletteShortcut(setOpen: (open: boolean) => void) {
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === "k") { event.preventDefault(); setOpen(true); }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [setOpen]);
}

// Mount once inside the shell: <CommandPalette session={session} />. It owns
// its shortcut, data, and dialog, so the shell can move it anywhere.
export function CommandPalette({ session }: { session: SessionIdentity }) {
  const [open, setOpen] = useState(false);
  usePaletteShortcut(setOpen);
  return <>
    <button type="button" className="icon-button palette-button" onClick={() => setOpen(true)} aria-label="Search and commands (⌘K)" title="Search and commands (⌘K / Ctrl+K)">
      <Search size={17} aria-hidden="true" /></button>
    {open ? <PaletteDialog session={session} onClose={() => setOpen(false)} /> : null}
  </>;
}

function PaletteDialog({ session, onClose }: { session: SessionIdentity; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();
  const pathname = useLocation({ select: (l) => l.pathname });
  const listId = useId();
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [active, setActive] = useState(0);
  const role = session.role;
  const org = session.organizationId;
  const recentKey = `blaxsmith-palette-recent:${org}:${session.principalId}`;
  const [recents, setRecents] = useState(() => readRecents(recentKey));
  const projectMatch = pathname.match(/^\/projects\/([^/]+)/)?.[1];
  const projectId = projectMatch && projectMatch !== "new" ? projectMatch : "";

  useEffect(() => { dialog.current?.showModal(); input.current?.focus(); }, []);
  useEffect(() => { const t = window.setTimeout(() => setSearch(query.trim()), 150); return () => window.clearTimeout(t); }, [query]);
  useEffect(() => { setActive(0); }, [query]);

  const admin = isAdmin(role);
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(projectId), queryFn: ({ signal }) => getProject(projectId, signal), retry: false });
  const projects = useQuery({ queryKey: ["projects", org, search, "palette"], queryFn: ({ signal }) => listProjects("", search, "created_at", "desc", signal), retry: false });
  const runs = useQuery({ queryKey: ["runs", org, projectId, search, "palette"], enabled: Boolean(projectId), retry: false,
    queryFn: ({ signal }) => listRuns(projectId, "", search, "created_at", "desc", signal) });
  const recipes = useQuery({ queryKey: ["recipes", org, projectId], queryFn: ({ signal }) => listRecipes(projectId, signal), retry: false });
  const personal = useQuery({ queryKey: ["connections", org, "personal", ""], queryFn: ({ signal }) => listConnections("personal", "", signal), retry: false });
  const orgConnections = useQuery({ queryKey: ["connections", org, "organization", ""], enabled: admin, queryFn: ({ signal }) => listConnections("organization", "", signal), retry: false });
  const users = useQuery({ queryKey: ["org-members", org], enabled: admin, queryFn: ({ signal }) => listMembers(signal), retry: false });
  const inbox = useQuery({ queryKey: ["admin-overview", org], enabled: admin, queryFn: ({ signal }) => getAdminOverview(signal), retry: false });

  const items = useMemo(() => paletteItems(role, {
    projects: projects.data?.projects, runs: runs.data?.runs, recipes: recipes.data?.recipes,
    connections: [...(personal.data ?? []), ...(admin ? orgConnections.data ?? [] : [])],
    users: admin ? users.data?.members : undefined, inbox: admin ? inbox.data?.openInteractions : undefined,
  }, { projectId, projectName: project.data?.project?.name }), [role, admin, projects.data, runs.data, recipes.data, personal.data, orgConnections.data, users.data, inbox.data, projectId, project.data]);
  const shown = useMemo(() => filterPalette(items, recents, query, role).slice(0, 60), [items, recents, query, role]);
  const loading = projects.isFetching || runs.isFetching;

  function choose(item: PaletteItem | undefined) {
    if (!item) return;
    const next = addRecent(recents, item);
    setRecents(next);
    try { localStorage.setItem(recentKey, JSON.stringify(next)); } catch { /* Storage unavailable. */ }
    onClose();
    const [path, hash] = item.to.split("#");
    const [pathOnly, queryString] = path.split("?");
    void navigate({ to: pathOnly as "/", search: Object.fromEntries(new URLSearchParams(queryString ?? "")) as never, hash });
  }

  function onKeyDown(event: ReactKeyboardEvent) {
    if (event.key === "ArrowDown") { event.preventDefault(); setActive((i) => Math.min(i + 1, shown.length - 1)); }
    else if (event.key === "ArrowUp") { event.preventDefault(); setActive((i) => Math.max(i - 1, 0)); }
    else if (event.key === "Home") { event.preventDefault(); setActive(0); }
    else if (event.key === "End") { event.preventDefault(); setActive(Math.max(shown.length - 1, 0)); }
    else if (event.key === "Enter") { event.preventDefault(); choose(shown[active]); }
  }
  useEffect(() => { document.getElementById(`${listId}-${active}`)?.scrollIntoView({ block: "nearest" }); }, [active, listId]);

  const groups = groupOrder.map((group) => ({ group, entries: shown.map((item, index) => ({ item, index })).filter((x) => x.item.group === group) })).filter((g) => g.entries.length);
  return <dialog ref={dialog} className="command-palette" aria-label="Search and commands" onClose={onClose}
    onClick={(event) => { if (event.target === dialog.current) dialog.current?.close(); }}>
    <div className="palette-search"><Search size={17} aria-hidden="true" />
      <input ref={input} value={query} onChange={(event) => setQuery(event.target.value)} onKeyDown={onKeyDown} placeholder="Search projects, runs, recipes, connections…"
        role="combobox" aria-expanded="true" aria-controls={listId} aria-autocomplete="list" aria-activedescendant={shown.length ? `${listId}-${active}` : undefined}
        aria-label="Search and commands" maxLength={120} autoComplete="off" spellCheck={false} />
      <kbd>Esc</kbd></div>
    <div id={listId} role="listbox" aria-label="Results" className="palette-results">
      {groups.map(({ group, entries }) => <div key={group} role="group" aria-label={group} className="palette-group">
        <div className="palette-group-label" aria-hidden="true">{group}</div>
        {entries.map(({ item, index }) => <div key={`${group}-${item.id}`} id={`${listId}-${index}`} role="option" aria-selected={index === active}
          className={index === active ? "palette-option is-active" : "palette-option"} onMouseMove={() => setActive(index)} onClick={() => choose(item)}>
          <span>{item.label}</span>{item.detail ? <small>{item.detail}</small> : null}</div>)}
      </div>)}
      {!shown.length ? <p className="palette-empty" role="status">{loading ? "Searching…" : query ? "No matches." : "No recent items yet."}</p> : null}
    </div>
    <p className="palette-hint"><kbd>↑</kbd><kbd>↓</kbd> move · <kbd>Enter</kbd> open · <kbd>Esc</kbd> close</p>
  </dialog>;
}
