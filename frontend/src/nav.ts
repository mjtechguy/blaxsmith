// Application information architecture: sidebar groups, active matching, and
// topbar breadcrumbs. Pure data so the IA can be tested per role without a DOM.
// Visibility only mirrors server RBAC: admin pages are also guarded by the
// /admin layout and every admin RPC is enforced on the server.
import {
  Activity, BookCopy, FolderKanban, GitBranch, GitPullRequest, House, Inbox, KeyRound, LayoutDashboard,
  ListChecks, Package, ScrollText, Settings, ShieldCheck, Timer, Users, Wrench, type LucideIcon,
} from "lucide-react";

export type NavItem = {
  id: string;
  label: string;
  href: string;
  icon: LucideIcon;
  exact?: boolean; // Active only on href itself (plus `also`).
  also?: string[]; // Extra path prefixes that mark this item active.
  children?: NavItem[];
  soon?: boolean; // Placeholder page, clearly labelled.
};

export type NavGroup = { id: string; label: string; items: NavItem[]; collapsible: boolean };

export type NavContext = {
  role?: string; // Session role; undefined when signed out.
  projectId?: string; // From the URL; the project group appears only with one.
  projectName?: string;
};

export const isOrgAdminRole = (role?: string) => role === "owner" || role === "admin";

export function projectIdFrom(pathname: string): string | undefined {
  const id = /^\/projects\/([^/]+)/.exec(pathname)?.[1];
  return id && id !== "new" ? decodeURIComponent(id) : undefined;
}

export function navigation({ role, projectId, projectName }: NavContext): NavGroup[] {
  if (!role) return [{ id: "library", label: "Library", collapsible: false, items: [
    { id: "tools", label: "Tools & runtimes", href: "/tools", icon: Wrench, exact: true },
  ] }];
  const groups: NavGroup[] = [
    { id: "home", label: "", collapsible: false, items: [{ id: "home", label: "Home", href: "/", icon: House, exact: true }] },
    { id: "work", label: "Work", collapsible: true, items: [
      { id: "inbox", label: "Inbox", href: "/inbox", icon: Inbox, exact: true },
      { id: "runs", label: "Runs", href: "/runs", icon: Activity, exact: true },
      { id: "projects", label: "Projects", href: "/projects", icon: FolderKanban, exact: true, also: ["/projects/new"] },
    ] },
  ];
  if (projectId) {
    const base = `/projects/${encodeURIComponent(projectId)}`;
    groups.push({ id: "project", label: projectName || "Project", collapsible: true, items: [
      { id: "project-overview", label: "Overview", href: base, icon: LayoutDashboard, exact: true, also: [`${base}/setup`] },
      { id: "project-runs", label: "Runs", href: `${base}/runs`, icon: GitBranch },
      { id: "project-recipes", label: "Project recipes", href: `${base}/recipes`, icon: BookCopy },
      { id: "project-connections", label: "Project connections", href: `${base}/connections`, icon: KeyRound, also: [`${base}/model-access`] },
      { id: "project-source", label: "Source & verification", href: `${base}/settings/source`, icon: ListChecks, also: [`${base}/settings/verification`] },
      { id: "project-settings", label: "Settings", href: `${base}/settings`, icon: Settings, exact: true },
    ] });
  }
  groups.push(
    { id: "library", label: "Library", collapsible: true, items: [
      { id: "library-recipes", label: "Recipes", href: "/recipes", icon: BookCopy },
      { id: "library-extensions", label: "Extensions", href: "/extensions", icon: Package },
      { id: "tools", label: "Tools & runtimes", href: "/tools", icon: Wrench, exact: true },
    ] },
  );
  if (isOrgAdminRole(role)) groups.push({ id: "admin", label: "Admin", collapsible: true, items: [
    { id: "admin-operations", label: "Operations", href: "/admin", icon: ShieldCheck, exact: true },
    { id: "admin-users", label: "Users", href: "/admin/users", icon: Users },
    { id: "admin-connections", label: "Connections", href: "/admin/connections", icon: KeyRound },
    { id: "admin-extensions", label: "Extensions", href: "/admin/extensions", icon: Package },
    { id: "admin-audit", label: "Audit", href: "/admin/audit", icon: ScrollText },
    { id: "admin-settings", label: "Settings", href: "/admin/settings", icon: Settings, children: [
      { id: "admin-github-app", label: "GitHub app", href: "/admin/settings/github-app", icon: GitPullRequest },
      { id: "admin-policies", label: "Policies", href: "/admin/settings/policies", icon: ShieldCheck, soon: true },
      { id: "admin-retention", label: "Retention", href: "/admin/settings/retention", icon: Timer, soon: true },
    ] },
  ] });
  return groups;
}

const trim = (path: string) => path.replace(/\/+$/, "") || "/";
const under = (path: string, prefix: string) => path === prefix || path.startsWith(`${prefix}/`);

// Whether the item itself matches (not counting children).
export function itemMatches(item: NavItem, pathname: string): boolean {
  const path = trim(pathname);
  if (item.also?.some((prefix) => under(path, prefix))) return true;
  return item.exact ? path === item.href : under(path, item.href);
}

// The single most specific active item, so parent and child highlight predictably:
// the longest matching href wins (a child over its parent, Settings over Overview).
export function activeItem(groups: NavGroup[], pathname: string): { group: NavGroup; item: NavItem; parent?: NavItem } | null {
  let best: { group: NavGroup; item: NavItem; parent?: NavItem; weight: number } | null = null;
  const consider = (group: NavGroup, item: NavItem, parent?: NavItem) => {
    if (!itemMatches(item, pathname)) return;
    const weight = Math.max(item.href.length, ...(item.also ?? []).filter((p) => under(trim(pathname), p)).map((p) => p.length));
    if (!best || weight > best.weight) best = { group, item, parent, weight };
  };
  for (const group of groups) for (const item of group.items) {
    consider(group, item);
    for (const child of item.children ?? []) consider(group, child, item);
  }
  if (!best) return null;
  const { group, item, parent } = best;
  return { group, item, parent };
}

export type Crumb = { label: string; href?: string };

// Breadcrumbs follow the IA: group › item › child, then an optional detail label
// (a run, connection, recipe, or user name) supplied by the shell.
export function breadcrumbs(groups: NavGroup[], pathname: string, detail?: string): Crumb[] {
  const active = activeItem(groups, pathname);
  // Account pages live in the user menu, not the sidebar.
  const path = trim(pathname);
  if (!active && under(path, "/me")) {
    const page = under(path, "/me/connections") ? { label: "My connections", href: "/me/connections" } : under(path, "/me/email") ? { label: "Set your email" } : { label: "Account settings", href: "/me/settings" };
    return [{ label: "Account" }, { label: page.label, href: detail || path !== page.href ? page.href : undefined }, ...(detail ? [{ label: detail }] : [])];
  }
  if (!active) return detail ? [{ label: detail }] : [{ label: "Blaxsmith" }];
  const crumbs: Crumb[] = [];
  if (active.group.label) crumbs.push({ label: active.group.label, href: active.group.id === "project" ? active.group.items[0]?.href : undefined });
  if (active.parent) crumbs.push({ label: active.parent.label, href: active.parent.href });
  const onItem = trim(pathname) === active.item.href;
  crumbs.push({ label: active.item.label, href: onItem && !detail ? undefined : active.item.href });
  if (detail) crumbs.push({ label: detail });
  return crumbs;
}

// Detail pages below a nav item get a trailing crumb.
export function detailKind(pathname: string): "run" | "new-run" | "connection" | "new-connection" | "recipe" | "new-recipe" | "user" | "new-user" | "new-project" | "setup" | "extension" | "new-extension" | undefined {
  const path = trim(pathname);
  if (path === "/projects/new") return "new-project";
  if (/^\/projects\/[^/]+\/setup$/.test(path)) return "setup";
  if (/\/runs\/new$/.test(path)) return "new-run";
  if (/\/runs\/[^/]+$/.test(path)) return "run";
  if (/\/connections\/new(\/|$)/.test(path)) return "new-connection";
  if (/\/connections\/[^/]+$/.test(path) && !path.endsWith("/github-app")) return "connection";
  if (/\/recipes\/new$/.test(path) || /\/recipes\/[^/]+\/versions\/new$/.test(path)) return "new-recipe";
  if (/\/recipes\/[^/]+$/.test(path)) return "recipe";
  if (path === "/admin/extensions/new") return "new-extension";
  if (/^(\/admin)?\/extensions\/[^/]+$/.test(path)) return "extension";
  if (path === "/admin/users/new") return "new-user";
  if (/^\/admin\/users\/[^/]+$/.test(path)) return "user";
  return undefined;
}

// Where a project switch lands: the same section of the other project, or its overview.
export function switchPath(pathname: string, nextProjectId: string): string {
  const base = `/projects/${encodeURIComponent(nextProjectId)}`;
  const section = /^\/projects\/[^/]+\/([^/]+)(\/([^/]+))?/.exec(pathname);
  if (!section) return base;
  if (section[1] === "settings") return section[3] ? `${base}/settings/${section[3]}` : `${base}/settings`;
  return ["runs", "recipes", "connections"].includes(section[1]) ? `${base}/${section[1]}` : base;
}

