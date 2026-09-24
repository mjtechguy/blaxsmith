import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { Building2, ChevronDown, ChevronRight, ChevronsUpDown, FolderKanban, Hammer, Menu, Moon, PanelLeftClose, PanelLeftOpen, Search, Sun, X } from "lucide-react";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";
import { activeItem, breadcrumbs, detailKind, itemMatches, navigation, projectIdFrom, switchPath, type NavGroup, type NavItem } from "./nav";
import { applyTheme, setPrefs, usePrefs, type Theme } from "./preferences";
import { Slot } from "./slots";
import "./setup-slots";
import { UserMenu } from "./user-menu";
import { getWorkspaceHome, homeKey } from "./workspace";
import { getProject, getRun, listProjects } from "./workflow";

// Display-only preferences, namespaced per product and version.
const COLLAPSED_KEY = "blaxsmith:nav:v1:collapsed";
const GROUPS_KEY = "blaxsmith:nav:v1:groups";
const read = <T,>(key: string, fallback: T): T => { try { const raw = localStorage.getItem(key); return raw === null ? fallback : JSON.parse(raw) as T; } catch { return fallback; } };
const write = (key: string, value: unknown) => { try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* storage unavailable */ } };

// Our IA decides what is current; TanStack only marks a link active on its exact path.
const exactPath = { exact: true, includeSearch: false } as const;

const detailLabels: Record<string, string> = {
  "new-run": "New run", connection: "Connection", "new-connection": "New connection", recipe: "Recipe", "new-recipe": "Recipe editor",
  user: "User", "new-user": "Invite user", "new-project": "New project", setup: "Setup",
};

export function Shell({ children, session }: { children: ReactNode; session?: SessionIdentity }) {
  const location = useLocation();
  const pathname = location.pathname;
  const navigate = useNavigate();
  const [collapsed, setCollapsed] = useState(() => read(COLLAPSED_KEY, false));
  const [mobileOpen, setMobileOpen] = useState(false);
  const { theme } = usePrefs();
  const menuButton = useRef<HTMLButtonElement>(null);
  const org = session?.organizationId ?? "";
  const scope = session ? `${session.organizationId}:${session.principalId}` : "";
  const projectId = session ? projectIdFrom(pathname) : undefined;
  const runId = /\/runs\/([^/]+)$/.exec(pathname)?.[1];
  const project = useQuery({ queryKey: ["project", org, projectId ?? ""], enabled: Boolean(org && projectId), queryFn: ({ signal }) => getProject(projectId!, signal) });
  const run = useQuery({ queryKey: ["run", scope, runId ?? ""], enabled: Boolean(scope && runId && runId !== "new"), queryFn: ({ signal }) => getRun(runId!, signal) });
  const home = useQuery({ queryKey: homeKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => getWorkspaceHome(signal), staleTime: 60_000 });
  const identity = home.data;
  const orgName = identity?.organizationName || "Organization";

  const groups = useMemo(() => navigation({ role: session?.role, projectId, projectName: project.data?.project?.name }), [session?.role, projectId, project.data?.project?.name]);
  const kind = detailKind(pathname);
  const detail = kind === "run" ? run.data?.run?.launchKey || "Run" : kind ? detailLabels[kind] : undefined;
  const crumbs = breadcrumbs(groups, pathname, detail);

  useEffect(() => { setMobileOpen(false); }, [pathname]);
  useEffect(() => { write(COLLAPSED_KEY, collapsed); }, [collapsed]);
  useEffect(() => {
    applyTheme(theme);
    const media = matchMedia("(prefers-color-scheme: dark)");
    const change = () => { if (theme === "system") applyTheme(theme); };
    media.addEventListener("change", change);
    return () => media.removeEventListener("change", change);
  }, [theme]);
  useEffect(() => {
    if (!mobileOpen) return;
    const close = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") { setMobileOpen(false); menuButton.current?.focus(); }
    };
    window.addEventListener("keydown", close);
    return () => window.removeEventListener("keydown", close);
  }, [mobileOpen]);

  const nextTheme: Theme = theme === "light" ? "dark" : theme === "dark" ? "system" : "light";

  return <div className={`app-frame ${collapsed ? "is-collapsed" : ""} ${mobileOpen ? "mobile-open" : ""}`}>
    <a className="skip-link" href="#main-content">Skip to content</a>
    <aside className="sidebar" id="primary-navigation" aria-label="Primary navigation">
      <div className="sidebar-brand">
        <Link to={session ? "/" : "/tools"} className="brand-link" aria-label="Blaxsmith home">
          <span className="brand-mark"><Hammer size={16} strokeWidth={2.3} aria-hidden="true" /></span>
          <span className="brand-copy"><strong>Blaxsmith</strong><small title={orgName}>{session ? orgName : "Engineering workspace"}</small></span>
        </Link>
        <button type="button" className="icon-button collapse-button" onClick={() => setCollapsed(!collapsed)} aria-label={collapsed ? "Expand navigation" : "Collapse navigation"} aria-controls="primary-navigation" aria-expanded={!collapsed}>
          {collapsed ? <PanelLeftOpen size={16} aria-hidden="true" /> : <PanelLeftClose size={16} aria-hidden="true" />}
        </button>
      </div>
      {session ? <div className="sidebar-context">
        {collapsed ? <Link to="/projects" className="nav-link context-icon" title={project.data?.project?.name ? `Project: ${project.data.project.name}` : "Projects"} aria-label={project.data?.project?.name ? `Project: ${project.data.project.name}. Open projects` : "Projects"}>
          <FolderKanban size={17} aria-hidden="true" /></Link>
          : <ProjectSwitcher org={org} currentId={projectId} currentName={project.data?.project?.name} pathname={pathname} />}
      </div> : null}
      <div className="sidebar-scroll">
        <nav aria-label="Main"><SidebarGroups groups={groups} pathname={pathname} collapsed={collapsed} /></nav>
      </div>
      <div className="sidebar-footer"><span className="footer-dot" aria-hidden="true" /> <span>Development preview</span></div>
    </aside>
    <div className="main-column">
      <header className="topbar">
        <div className="topbar-left">
          <button type="button" ref={menuButton} className="icon-button menu-button" onClick={() => setMobileOpen(!mobileOpen)} aria-label={mobileOpen ? "Close navigation" : "Open navigation"} aria-expanded={mobileOpen} aria-controls="primary-navigation">
            {mobileOpen ? <X size={19} aria-hidden="true" /> : <Menu size={19} aria-hidden="true" />}</button>
          <nav className="breadcrumb" aria-label="Breadcrumb"><ol>
            {crumbs.map((crumb, index) => <li key={`${crumb.label}-${index}`} className={index === crumbs.length - 1 ? "crumb-current" : undefined}>
              {index > 0 ? <ChevronRight size={13} aria-hidden="true" /> : null}
              {crumb.href && index < crumbs.length - 1 ? <Link to={crumb.href as "/"} activeOptions={exactPath}>{crumb.label}</Link> : <span aria-current={index === crumbs.length - 1 ? "page" : undefined}>{crumb.label}</span>}
            </li>)}
          </ol></nav>
        </div>
        <div className="topbar-right">
          {session ? <Slot name="topbar.command" /> : null}
          {session ? <UserMenu /> : <>
            <button type="button" className="icon-button theme-button" onClick={() => setPrefs({ theme: nextTheme })} aria-label={`Theme: ${theme}. Switch to ${nextTheme}`} title={`Theme: ${theme}`}>
              {theme === "dark" ? <Moon size={17} aria-hidden="true" /> : <Sun size={17} aria-hidden="true" />}
            </button>
            <Link to="/login" search={{ next: "/tools" }} className="secondary-button">Sign in</Link></>}
        </div>
      </header>
      <main id="main-content" className="main-content" tabIndex={-1}>{children}</main>
    </div>
  </div>;
}

function SidebarGroups({ groups, pathname, collapsed }: { groups: NavGroup[]; pathname: string; collapsed: boolean }) {
  const [openGroups, setOpenGroups] = useState<Record<string, boolean>>(() => read(GROUPS_KEY, {}));
  const active = activeItem(groups, pathname);
  const toggle = (id: string, value: boolean) => { const next = { ...openGroups, [id]: value }; setOpenGroups(next); write(GROUPS_KEY, next); };
  return <>{groups.map((group) => {
    // A group holding the current page is always open, so the active item stays visible.
    const holdsActive = active?.group.id === group.id;
    const isOpen = !group.collapsible || collapsed || holdsActive || openGroups[group.id] !== false;
    const listId = `nav-group-${group.id}`;
    return <div key={group.id} className={`nav-group${group.id === "project" ? " nav-group-project" : ""}`}>
      {group.label && !collapsed ? group.collapsible
        ? <button type="button" className="nav-group-toggle" aria-expanded={isOpen} aria-controls={listId} disabled={holdsActive}
          title={holdsActive ? "Contains the current page" : undefined} onClick={() => toggle(group.id, !isOpen)}>
          <span className="nav-group-label">{group.label}</span><ChevronDown size={14} aria-hidden="true" className={isOpen ? "is-open" : undefined} /></button>
        : <p className="nav-caption">{group.label}</p> : null}
      {collapsed && group.label ? <span className="nav-divider" aria-hidden="true" /> : null}
      {isOpen ? <ul id={listId} className="nav-list" aria-label={group.label || "Home"}>
        {group.items.map((item) => <NavEntry key={item.id} item={item} activeId={active?.item.id} parentId={active?.parent?.id} pathname={pathname} collapsed={collapsed} />)}
      </ul> : null}
    </div>;
  })}</>;
}

function NavEntry({ item, activeId, parentId, pathname, collapsed }: { item: NavItem; activeId?: string; parentId?: string; pathname: string; collapsed: boolean }) {
  const isActive = activeId === item.id;
  const holdsActive = parentId === item.id;
  const [childrenOpen, setChildrenOpen] = useState(holdsActive || itemMatches(item, pathname));
  useEffect(() => { if (holdsActive) setChildrenOpen(true); }, [holdsActive]);
  const Icon = item.icon;
  const childList = `nav-children-${item.id}`;
  return <li>
    <div className={`nav-row${item.children && !collapsed ? " has-children" : ""}`}>
      <Link to={item.href as "/"} activeOptions={exactPath} className={`nav-link${isActive ? " active" : ""}${holdsActive ? " parent-active" : ""}`} aria-current={isActive ? "page" : undefined}
        title={collapsed ? item.label : undefined} aria-label={collapsed ? item.label : undefined}>
        <Icon size={17} aria-hidden="true" /><span className="nav-text">{item.label}</span>
      </Link>
      {item.children && !collapsed ? <button type="button" className="nav-expand" aria-expanded={childrenOpen} aria-controls={childList}
        aria-label={`${childrenOpen ? "Hide" : "Show"} ${item.label} pages`} onClick={() => setChildrenOpen(!childrenOpen)}>
        <ChevronDown size={14} aria-hidden="true" className={childrenOpen ? "is-open" : undefined} /></button> : null}
    </div>
    {item.children && childrenOpen && !collapsed ? <ul id={childList} className="nav-sublist">
      {item.children.map((child) => <li key={child.id}>
        <Link to={child.href as "/"} activeOptions={exactPath} className={`nav-link nav-child${activeId === child.id ? " active" : ""}`} aria-current={activeId === child.id ? "page" : undefined}>
          <span className="nav-text">{child.label}</span>{child.soon ? <span className="soon-badge">Soon</span> : null}</Link>
      </li>)}
    </ul> : null}
  </li>;
}

// Searchable project context (ARIA combobox with a listbox). Selecting a project
// puts it in the URL; the Project navigation group follows the URL.
function ProjectSwitcher({ org, currentId, currentName, pathname }: { org: string; currentId?: string; currentName?: string; pathname: string }) {
  const navigate = useNavigate();
  const [isOpen, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => { const t = window.setTimeout(() => setQuery(search.trim()), 200); return () => window.clearTimeout(t); }, [search]);
  const projects = useQuery({ queryKey: ["projects", org, query, "switcher"], enabled: Boolean(org && isOpen),
    queryFn: ({ signal }) => listProjects("", query, query ? "name" : "created_at", query ? "asc" : "desc", signal) });
  const options = projects.data?.projects ?? [];
  useEffect(() => { setActiveIndex(0); }, [query]);
  useEffect(() => {
    if (!isOpen) return;
    const outside = (event: MouseEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", outside);
    return () => document.removeEventListener("mousedown", outside);
  }, [isOpen]);
  const choose = (projectId: string) => { setOpen(false); setSearch(""); button.current?.focus(); void navigate({ to: switchPath(pathname, projectId) as "/" }); };
  const onKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown") { event.preventDefault(); setActiveIndex((i) => Math.min(options.length - 1, i + 1)); }
    else if (event.key === "ArrowUp") { event.preventDefault(); setActiveIndex((i) => Math.max(0, i - 1)); }
    else if (event.key === "Enter" && options[activeIndex]) { event.preventDefault(); choose(options[activeIndex].id); }
    else if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); setOpen(false); button.current?.focus(); }
  };
  return <div className="switcher" ref={root}>
    <span className="switcher-label" id={`${id}-label`}>Project</span>
    <button ref={button} type="button" className="switcher-button" aria-haspopup="listbox" aria-expanded={isOpen} aria-labelledby={`${id}-label ${id}-value`} onClick={() => setOpen(!isOpen)}>
      <Building2 size={15} aria-hidden="true" /><span id={`${id}-value`} className={currentId ? undefined : "muted"}>{currentId ? currentName || "Loading…" : "Select a project"}</span><ChevronsUpDown size={14} aria-hidden="true" />
    </button>
    {isOpen ? <div className="switcher-panel">
      <label className="search-field switcher-search"><Search size={14} aria-hidden="true" /><span className="sr-only">Find a project</span>
        <input autoFocus role="combobox" aria-expanded="true" aria-controls={`${id}-list`} aria-autocomplete="list" placeholder="Find a project"
          aria-activedescendant={options[activeIndex] ? `${id}-opt-${options[activeIndex].id}` : undefined} value={search} onChange={(event) => setSearch(event.target.value)} onKeyDown={onKey} maxLength={120} /></label>
      <ul id={`${id}-list`} role="listbox" aria-label="Projects" className="switcher-list">
        {projects.isPending ? <li className="switcher-note" role="presentation">Loading projects…</li> : null}
        {projects.isError ? <li className="switcher-note" role="presentation">Projects could not be loaded.</li> : null}
        {projects.isSuccess && !options.length ? <li className="switcher-note" role="presentation">No projects match.</li> : null}
        {options.map((p, index) => <li key={p.id} id={`${id}-opt-${p.id}`} role="option" aria-selected={p.id === currentId}
          className={`switcher-option${index === activeIndex ? " is-active" : ""}`} onMouseEnter={() => setActiveIndex(index)} onMouseDown={(event) => { event.preventDefault(); choose(p.id); }}>
          <span>{p.name}</span><small>{p.slug}</small></li>)}
      </ul>
      <Link to="/projects" className="text-action switcher-all" onClick={() => setOpen(false)}>All projects</Link>
    </div> : null}
  </div>;
}
