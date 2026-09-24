// Command palette model: what a role may see, how items match a query, and
// the per-user recent list. The shape follows t3code's CommandPalette.logic
// (groups, ranked filtering, recents); the code is Blaxsmith's own.

export type PaletteGroup = "Recent" | "Actions" | "Inbox" | "Projects" | "Runs" | "Recipes" | "Connections" | "Users";
export type PaletteItem = { id: string; group: PaletteGroup; label: string; detail?: string; to: string; keywords?: string };

export type PaletteData = {
  projects?: Array<{ id: string; name: string; slug: string }>;
  runs?: Array<{ id: string; projectId: string; launchKey: string; state: string }>;
  recipes?: Array<{ id: string; name: string; projectId: string; currentVersionId: string }>;
  connections?: Array<{ id: string; scope: string; ownerId: string; provider: string; kind: string; label: string }>;
  users?: Array<{ principalId: string; username: string; displayName: string; email?: string; role: string }>;
  inbox?: Array<{ id: string; runId: string; projectId: string; projectName: string; title: string; kind: string; stage?: string }>;
};

// Where an Inbox item opens: the run's review tab, the interview's stage, or the run's inbox.
export function inboxTarget(i: { runId: string; projectId: string; kind: string; stage?: string }): string {
  const run = `/projects/${i.projectId}/runs/${i.runId}`;
  if (i.kind === "review") return `${run}?tab=review`;
  if (i.kind === "interview_round") return `${run}?tab=stages${i.stage ? `&stage=${encodeURIComponent(i.stage)}` : ""}`;
  return `${run}#inbox-heading`;
}

const isAdmin = (role: string) => role === "owner" || role === "admin";
const mayLaunch = (role: string) => isAdmin(role) || role === "member";

// Everything the palette may offer this role. It mirrors server RBAC so the
// palette never offers what the server would deny: users and organization
// connections are admin-only, launching and creating projects need member or
// above, and project connection detail needs project administration (org
// admins here; project creators reach theirs from the project page).
export function paletteItems(role: string, data: PaletteData, context: { projectId?: string; projectName?: string } = {}): PaletteItem[] {
  const out: PaletteItem[] = [];
  const action = (id: string, label: string, to: string, keywords = "") => out.push({ id: `action:${id}`, group: "Actions", label, to, keywords });
  if (mayLaunch(role)) action("new-project", "New project", "/projects/new", "create workspace add");
  action("new-api-key", "New API key (personal)", "/me/connections/new/api-key", "connection key model");
  if (isAdmin(role)) {
    action("new-org-api-key", "New organization API key", "/admin/connections/new/api-key", "connection key model");
    action("connect-github", "Connect GitHub", "/admin/connections/new/git", "git oauth repository");
  }
  action("inbox", "Inbox: items awaiting me", "/inbox", "questions approvals reviews waiting");
  action("runs", "Runs: all runs", "/runs", "activity history launched");
  if (context.projectId) action("project-runs", `Runs in ${context.projectName || "this project"}`, `/projects/${context.projectId}/runs`, "activity history launched");
  if (mayLaunch(role)) {
    if (context.projectId) {
      for (const r of data.recipes ?? []) {
        if (!r.currentVersionId) continue;
        action(`run-recipe:${r.id}`, `Start run from recipe: ${r.name}`, `/projects/${context.projectId}/runs/new?recipe=${encodeURIComponent(r.id)}`, "launch new run");
      }
    }
    for (const p of (data.projects ?? []).slice(0, 5)) {
      if (p.id !== context.projectId) action(`run-in:${p.id}`, `Start run in ${p.name}…`, `/projects/${p.id}/runs/new`, "launch new run recipe");
    }
  }
  // The caller's own actionable Inbox (WorkspaceService.ListInbox), for every role.
  for (const i of data.inbox ?? []) {
    out.push({ id: `inbox:${i.id}`, group: "Inbox", label: i.title || i.kind, detail: `${i.projectName} · ${i.kind.replaceAll("_", " ")}`, to: inboxTarget(i) });
  }
  for (const p of data.projects ?? []) out.push({ id: `project:${p.id}`, group: "Projects", label: p.name, detail: p.slug, to: `/projects/${p.id}` });
  for (const r of data.runs ?? []) {
    out.push({ id: `run:${r.id}`, group: "Runs", label: r.launchKey, detail: `${context.projectName ?? "Run"} · ${r.state.replaceAll("_", " ")}`, to: `/projects/${r.projectId}/runs/${r.id}` });
  }
  for (const r of data.recipes ?? []) {
    // Outside a project, recipes open in Library › Recipes, which every member can read.
    const to = context.projectId ? `/projects/${context.projectId}/recipes/${r.id}` : `/recipes/${r.id}`;
    out.push({ id: `recipe:${r.id}`, group: "Recipes", label: r.name, detail: r.projectId ? "Project recipe" : "Organization recipe", to });
  }
  for (const c of data.connections ?? []) {
    if (c.scope === "organization" && !isAdmin(role)) continue;
    const to = c.scope === "organization" ? `/admin/connections/${c.id}` : c.scope === "personal" ? `/me/connections/${c.id}` : `/projects/${c.ownerId}/connections/${c.id}`;
    out.push({ id: `connection:${c.id}`, group: "Connections", label: c.label || c.provider, detail: `${c.scope} · ${c.kind.replace("_", " ")}`, to, keywords: c.provider });
  }
  if (isAdmin(role)) {
    for (const u of data.users ?? []) {
      out.push({ id: `user:${u.principalId}`, group: "Users", label: u.displayName || u.email || u.username, detail: `${u.email || u.username} · ${u.role}`, to: "/admin/users", keywords: `${u.email ?? ""} ${u.username}` });
    }
  }
  return out;
}

// Lower is better; null means no match. Every whitespace-separated token
// must match the label, detail, keywords, or group.
export function matchScore(item: PaletteItem, query: string): number | null {
  const tokens = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (!tokens.length) return 0;
  const label = item.label.toLowerCase();
  const rest = `${item.detail ?? ""} ${item.keywords ?? ""} ${item.group}`.toLowerCase();
  let score = 0;
  for (const token of tokens) {
    if (label.startsWith(token)) score += 0;
    else if (label.includes(` ${token}`) || label.includes(`-${token}`) || label.includes(`/${token}`)) score += 2;
    else if (label.includes(token)) score += 4;
    else if (rest.includes(token)) score += 8;
    else return null;
  }
  return score;
}

export const groupOrder: PaletteGroup[] = ["Recent", "Actions", "Inbox", "Projects", "Runs", "Recipes", "Connections", "Users"];

// Empty query: recents first, then actions. Otherwise ranked matches, stable
// within equal scores, grouped in group order.
// Route-level guard for stored recents (they may predate a role change):
// admin pages need owner/admin, creating projects and runs needs member+.
export function allowedForRole(item: Pick<PaletteItem, "to">, role: string): boolean {
  if (item.to.startsWith("/admin")) return isAdmin(role);
  if (item.to === "/projects/new" || item.to.includes("/runs/new")) return mayLaunch(role);
  return Boolean(role);
}

export function filterPalette(items: PaletteItem[], recents: PaletteItem[], query: string, role: string): PaletteItem[] {
  if (!query.trim()) {
    const recent = recents.filter((r) => allowedForRole(r, role)).map((r) => ({ ...r, group: "Recent" as const }));
    return [...recent, ...items.filter((i) => i.group === "Actions" && !recent.some((r) => r.id === i.id))];
  }
  return items.map((item, index) => ({ item, index, score: matchScore(item, query) }))
    .filter((x): x is { item: PaletteItem; index: number; score: number } => x.score !== null)
    .sort((a, b) => groupOrder.indexOf(a.item.group) - groupOrder.indexOf(b.item.group) || a.score - b.score || a.index - b.index)
    .map((x) => x.item);
}

export const RECENT_LIMIT = 12;

export function addRecent(recents: PaletteItem[], item: PaletteItem): PaletteItem[] {
  const { group, ...rest } = item;
  const original = group === "Recent" ? recents.find((r) => r.id === item.id)?.group ?? "Actions" : group;
  return [{ ...rest, group: original }, ...recents.filter((r) => r.id !== item.id)].slice(0, RECENT_LIMIT);
}
