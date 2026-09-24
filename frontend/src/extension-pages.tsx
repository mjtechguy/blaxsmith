// Extension pages on the release templates. Admin › Extensions (owners and
// admins): a CollectionTable of every installed extension, a DetailLayout with
// Overview · Versions · Permissions · Templates · Grants, and a CreateFlow
// install (source → preview → permissions → install). Library › Extensions
// (every member): the extensions granted to the projects they can see, read
// only. The server enforces who may change what.
import { useMemo, useState, type ReactNode } from "react";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, Check, Download, FileJson, Package, Plus, RefreshCw } from "lucide-react";
import { isOrgAdmin } from "./admin";
import { currentSession, sessionQueryKey } from "./auth";
import { failure, ResourceGrants } from "./connection-ui";
import { CollectionTable, DataTable, inSet, useUrlView, type GridColumn } from "./data-table";
import { TextField } from "./form-field";
import type { Extension, ExtensionPermission, ExtensionTemplate, ExtensionVersion, PreviewExtensionInstallResponse } from "./gen/blaxsmith/api/v1/extensions_pb";
import { CreateFlow, DetailLayout, SummaryList, type FlowStep } from "./layouts";
import { PageHeader, PageShell } from "./page";
import { Card, CopyValue, Disclosure, EmptyState, StatePanel, tabFrom, Timestamp, type TabSpec } from "./ui";
import {
  approvedPermissions, checkExtensionUpdate, extensionKey, extensionsKey, getExtension, grantExtension, installExtension, listExtensions,
  permissionKinds, previewExtensionInstall, revokeExtensionGrant, type ExtensionSourceInput,
} from "./extensions";
import { listProjects } from "./workflow";

function useOrg() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return { org: session.data?.organizationId || "", admin: isOrgAdmin(session.data), ready: session.isSuccess };
}

const short = (commit: string) => commit.slice(0, 12);
const updateState = (e: Extension) => e.updateAvailable ? "available" : e.latestCheckedAt ? "current" : "unchecked";
const updateOptions = [{ value: "available", label: "Update available" }, { value: "current", label: "Up to date" }, { value: "unchecked", label: "Not checked" }];

function UpdateBadge({ extension }: { extension: Extension }) {
  const state = updateState(extension);
  return state === "available" ? <span className="state-badge state-waiting" title={`The ref now resolves to ${extension.latestRefCommit}`}>Update available</span>
    : <span className="state-badge">{state === "current" ? "Up to date" : "Not checked"}</span>;
}

// Library rows: an installed extension plus the projects it is granted to.
type ExtensionRow = Extension & { projects: string[] };

function ExtensionLink({ extension, library }: { extension: Extension; library: boolean }) {
  const body = <><span className="project-symbol"><Package size={15} aria-hidden="true" /></span>
    <span><strong>{extension.key}</strong><small className="mono">{extension.repositoryUrl.replace(/^https:\/\//, "")} @ {extension.gitRef}</small></span></>;
  return library ? <Link to="/extensions/$extensionId" params={{ extensionId: extension.id }} className="run-link">{body}</Link>
    : <Link to="/admin/extensions/$extensionId" params={{ extensionId: extension.id }} className="run-link">{body}</Link>;
}

function useExtensionColumns(library: boolean) {
  return useMemo<GridColumn<ExtensionRow>[]>(() => [
    { id: "key", accessorKey: "key", header: "Extension", enableHiding: false, cell: ({ row }) => <ExtensionLink extension={row.original} library={library} /> },
    { id: "current", accessorKey: "currentVersion", header: "Current", cell: ({ row }) => row.original.currentVersion
      ? <span>v{row.original.currentVersion}<br /><small className="mono">{short(row.original.currentCommit)}</small></span> : <span className="muted">—</span> },
    ...(library ? [{ id: "projects", accessorFn: (r: ExtensionRow) => r.projects.join(", "), header: "Granted to", enableSorting: false,
      cell: ({ row }: { row: { original: ExtensionRow } }) => row.original.projects.join(", ") } as GridColumn<ExtensionRow>] : []),
    { id: "versions", accessorKey: "versionCount", header: "Versions" },
    { id: "update", accessorFn: updateState, header: "Update", filterFn: inSet, cell: ({ row }) => <UpdateBadge extension={row.original} /> },
    { id: "checked", accessorKey: "latestCheckedAt", header: "Checked", cell: ({ row }) => row.original.latestCheckedAt ? <Timestamp value={row.original.latestCheckedAt} /> : <span className="muted">Never</span> },
  ], [library]);
}

// Admin › Extensions: every installed extension.
export function ExtensionListPage() {
  const { org, admin } = useOrg();
  const extensions = useQuery({ queryKey: extensionsKey(org), enabled: Boolean(org), queryFn: ({ signal }) => listExtensions("", signal) });
  const [view, setView] = useUrlView({ sort: [{ id: "key", desc: false }], size: 20 });
  const columns = useExtensionColumns(false);
  const rows = useMemo<ExtensionRow[]>(() => (extensions.data?.extensions ?? []).map((e) => ({ ...e, projects: [] })), [extensions.data]);
  const install = admin ? <Link to="/admin/extensions/new" className="primary-button"><Plus size={15} aria-hidden="true" /> Install extension</Link> : null;
  return <PageShell>
    <PageHeader title="Extensions" actions={install}
      description="Extension packs pinned to one Git commit. Stage templates run only inside sandboxes, with the permissions approved at install; projects use them only through a grant." />
    <section className="table-section" aria-label="Extensions">
      <CollectionTable id="admin-extensions" label="Extensions" noun="extensions" columns={columns} data={rows} getRowId={(e) => e.id}
        view={view} onView={setView} pinFirst searchLabel="Search extensions" facets={[{ id: "update", label: "Update", options: updateOptions }]}
        loading={extensions.isPending} refreshing={extensions.isFetching && !extensions.isPending}
        error={extensions.isError ? <>Extensions could not be loaded. <button type="button" className="text-action" onClick={() => void extensions.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState icon={<Package size={22} aria-hidden="true" />} title="No extensions installed" action={install}>Install one from a public GitHub or GitLab repository.</EmptyState>} />
    </section>
  </PageShell>;
}

// Library › Extensions: extensions granted to the projects the member can see.
export function ExtensionLibraryPage() {
  const { org } = useOrg();
  const projects = useQuery({ queryKey: ["projects", org, "", "extension-library"], enabled: Boolean(org), queryFn: ({ signal }) => listProjects("", "", "name", "asc", signal) });
  const perProject = useQueries({ queries: (projects.data?.projects ?? []).map((p) => ({
    queryKey: extensionsKey(org, p.id), queryFn: ({ signal }: { signal: AbortSignal }) => listExtensions(p.id, signal) })) });
  const [view, setView] = useUrlView({ sort: [{ id: "key", desc: false }], size: 20 });
  const columns = useExtensionColumns(true);
  const loading = projects.isPending || perProject.some((q) => q.isPending);
  const failed = projects.isError || perProject.some((q) => q.isError);
  const byId = new Map<string, ExtensionRow>();
  (projects.data?.projects ?? []).forEach((p, i) => {
    for (const e of perProject[i]?.data?.extensions ?? []) {
      const row = byId.get(e.id) ?? { ...e, projects: [] };
      byId.set(e.id, { ...row, projects: [...row.projects, p.name] });
    }
  });
  const rows = [...byId.values()];
  return <PageShell>
    <PageHeader title="Extensions"
      description="Installed extensions granted to your projects. Recipe stages name their templates as extension@version/template; runs freeze the exact version and approved permissions." />
    <section className="table-section" aria-label="Extensions">
      <CollectionTable id="library-extensions" label="Extensions" noun="extensions" columns={columns} data={rows} getRowId={(e) => e.id}
        view={view} onView={setView} pinFirst searchLabel="Search extensions" loading={loading}
        error={failed ? <>Extensions could not be loaded. <button type="button" className="text-action" onClick={() => void projects.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState icon={<Package size={22} aria-hidden="true" />} title="No extensions granted">An owner or admin installs extensions and grants them to projects, members, or roles.</EmptyState>} />
    </section>
  </PageShell>;
}

const permissionFeatures = tableFeatures({});
const templateFeatures = tableFeatures({});

const templateColumns: ColumnDef<typeof templateFeatures, ExtensionTemplate>[] = [
  { id: "id", header: "Template", cell: ({ row }) => <span><strong>{row.original.title || row.original.id}</strong><br /><code>{row.original.reference}</code></span> },
  { id: "mode", header: "Mode", cell: ({ row }) => <span className="state-badge">{row.original.mode}</span> },
  { id: "harness", header: "Harness", cell: ({ row }) => <span className="mono">{row.original.harness}</span> },
  { id: "kinds", header: "Fills stages", cell: ({ row }) => <span className="mono">{row.original.kinds.join(", ")}</span> },
];

function Templates({ templates, note }: { templates: ExtensionTemplate[]; note?: ReactNode }) {
  const table = useTable({ features: templateFeatures, data: templates, columns: templateColumns, getRowId: (row) => row.id });
  return <section className="table-section" aria-labelledby="templates-heading">
    <div className="table-heading"><div><h2 id="templates-heading">Stage templates</h2><p>{note ?? <>Name a template in a recipe stage as <code>extension@version/template</code>, or pick it in the recipe editor.</>}</p></div></div>
    <DataTable table={table} label="Stage templates" empty="No stage templates." />
  </section>;
}

// Declared permissions. With choose, required ones stay locked on and optional
// ones can be declined; otherwise approved lists what this version was granted.
function Permissions({ permissions, approved, chosen, choose, note }: {
  permissions: ExtensionPermission[]; approved?: string[]; chosen?: ReadonlySet<string>; choose?: (id: string, on: boolean) => void; note?: ReactNode;
}) {
  const columns = useMemo<ColumnDef<typeof permissionFeatures, ExtensionPermission>[]>(() => [
    { id: "approve", header: choose ? "Approve" : "Approved", cell: ({ row }) => {
      const p = row.original;
      if (!choose) return approved?.includes(p.id) ? <span className="state-badge state-succeeded"><Check size={12} aria-hidden="true" /> Approved</span> : <span className="state-badge">Declined</span>;
      return <label className="recipe-check"><input type="checkbox" checked={!p.optional || Boolean(chosen?.has(p.id))} disabled={!p.optional}
        onChange={(event) => choose(p.id, event.target.checked)} aria-label={`Approve ${p.id}`} /> {p.optional ? "Optional" : "Required"}</label>;
    } },
    { id: "kind", header: "Kind", cell: ({ row }) => permissionKinds[row.original.kind] || row.original.kind },
    { id: "id", header: "Permission", cell: ({ row }) => <span><code>{row.original.id}</code><br /><small className="admin-wrap">{row.original.description}</small></span> },
  ], [approved, chosen, choose]);
  const table = useTable({ features: permissionFeatures, data: permissions, columns, getRowId: (row) => row.id });
  return <section className="table-section" aria-labelledby="permissions-heading">
    <div className="table-heading"><div><h2 id="permissions-heading">Permissions</h2><p>{note ?? (choose
      ? "Everything this extension may do inside a sandbox. Required permissions cannot be declined; declined optional ones never reach a run."
      : "What this version was installed with. Runs freeze this list and its digest.")}</p></div></div>
    <DataTable table={table} label="Extension permissions" empty="This extension declares no permissions." />
  </section>;
}

type InstallStep = "source" | "preview" | "permissions" | "install";
const installSteps: Array<{ id: InstallStep; label: string }> = [
  { id: "source", label: "Source" }, { id: "preview", label: "Preview & validate" }, { id: "permissions", label: "Approve permissions" }, { id: "install", label: "Install" },
];

export function ExtensionInstallPage({ initial }: { initial: Partial<ExtensionSourceInput> }) {
  const { org, admin, ready } = useOrg();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [source, setSource] = useState<ExtensionSourceInput>({ repositoryUrl: initial.repositoryUrl || "", gitRef: initial.gitRef || "main",
    manifestPath: initial.manifestPath || "", overlayManifestJson: "" });
  const [step, setStep] = useState<InstallStep>("source");
  const [preview, setPreview] = useState<PreviewExtensionInstallResponse | null>(null);
  const [previewed, setPreviewed] = useState<ExtensionSourceInput | null>(null);
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  // Any source edit discards the preview: install pins only the previewed commit.
  const edit = (field: keyof ExtensionSourceInput) => (value: string) => { setSource((s) => ({ ...s, [field]: value })); setPreview(null); setStep("source"); };
  const valid = Boolean(preview && preview.errors.length === 0 && preview.commit);
  const approved = preview ? approvedPermissions(preview.permissions, chosen) : [];
  const at = installSteps.findIndex((s) => s.id === step);
  const steps: FlowStep[] = installSteps.map((s, i) => ({ id: s.id, label: s.label, state: i < at ? "done" : i === at ? "current" : "todo" }));

  async function runPreview() {
    setBusy("preview"); setError("");
    try {
      const result = await previewExtensionInstall(source);
      setPreview(result); setPreviewed(source); setChosen(new Set()); setStep("preview");
    } catch (cause) {
      setError(failure(cause, "The repository could not be previewed. Check the URL and ref, then try again."));
    } finally { setBusy(""); }
  }

  async function install() {
    if (!preview || !previewed) return;
    setBusy("install"); setError("");
    try {
      const result = await installExtension(previewed, preview.commit, approved);
      if (result.errors.length) { setPreview({ ...preview, errors: result.errors }); setStep("preview"); return; }
      await queryClient.invalidateQueries({ queryKey: ["extensions", org] });
      if (result.extension) await navigate({ to: "/admin/extensions/$extensionId", params: { extensionId: result.extension.id } });
    } catch (cause) {
      setError(failure(cause, "The extension could not be installed. Preview again and retry."));
    } finally { setBusy(""); }
  }

  const summary = <><h2>Summary</h2><SummaryList items={[
    { label: "Repository", value: source.repositoryUrl ? <span className="mono">{source.repositoryUrl.replace(/^https:\/\//, "")}</span> : "—", done: Boolean(source.repositoryUrl) },
    { label: "Ref", value: source.gitRef || "—" },
    { label: "Pinned commit", value: valid && preview ? <CopyValue value={preview.commit} label="Pinned commit" chars={12} /> : "After preview", done: valid },
    { label: "Extension", value: valid && preview ? `${preview.extensionKey} v${preview.version}` : "—" },
    { label: "Permissions", value: valid && preview ? `${approved.length} of ${preview.permissions.length} approved` : "—", done: at >= 3 ? true : undefined },
  ]} /><p className="form-hint">Preview resolves the ref to one commit and validates the manifest there. Install pins that exact commit; if the ref moves in between, install refuses and asks for a new preview.</p></>;

  if (ready && !admin) return <StatePanel kind="note" title="Installing is restricted">Organization owners and admins install extensions.</StatePanel>;
  return <CreateFlow title="Install extension" description="Add an extension pack from a public Git repository at one pinned commit." steps={steps} summary={summary}
    back={{ href: "/admin/extensions", label: "All extensions" }}>
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    {step === "source" ? <section className="editor-card" aria-labelledby="source-heading">
      <div className="editor-card-heading"><span className="project-symbol"><Package size={18} aria-hidden="true" /></span><div>
        <h2 id="source-heading">Source</h2><p>A public GitHub or GitLab HTTPS repository. Private repositories are not supported yet.</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); void runPreview(); }}>
        <TextField label="Repository URL" name="repositoryUrl" type="url" autoComplete="off" placeholder="https://github.com/owner/repo" value={source.repositoryUrl} onChange={edit("repositoryUrl")} onBlur={() => undefined} />
        <TextField label="Git ref" name="gitRef" autoComplete="off" placeholder="main, a tag, or a commit" value={source.gitRef} onChange={edit("gitRef")} onBlur={() => undefined} />
        <TextField label="Manifest path" name="manifestPath" autoComplete="off" placeholder="blaxsmith-extension.json" value={source.manifestPath} onChange={edit("manifestPath")} onBlur={() => undefined} required={false} />
        <Disclosure defaultOpen={Boolean(source.overlayManifestJson)} summary={<><FileJson size={14} aria-hidden="true" /> Supply the manifest (for a repository without one)</>}>
          <label className="recipe-json form-field"><span>Overlay manifest JSON</span>
            <textarea spellCheck={false} rows={14} value={source.overlayManifestJson} onChange={(event) => edit("overlayManifestJson")(event.target.value)} /></label></Disclosure>
        <div className="editor-actions">
          <Link to="/admin/extensions" className="secondary-button">Cancel</Link>
          <button className="primary-button" type="submit" disabled={Boolean(busy) || !source.repositoryUrl.trim() || !source.gitRef.trim()}>
            <RefreshCw size={15} aria-hidden="true" className={busy === "preview" ? "spin" : undefined} /> {busy === "preview" ? "Previewing…" : "Preview"}</button>
        </div>
      </form>
    </section> : null}
    {step === "preview" && preview ? <>
      {preview.errors.length ? <div className="recipe-validation is-invalid" role="alert">
        {preview.errors.map((e) => <span key={`${e.path}:${e.message}`}><code>{e.path}</code> {e.message}</span>)}</div>
        : <p className="notice" role="status"><strong>{preview.extensionKey} v{preview.version}</strong> validates at commit <code>{short(preview.commit)}</code> · manifest sha256 <code>{short(preview.manifestSha256)}</code>
          {preview.existingExtensionId ? <> · already installed; this adds a version. <Link className="text-action" to="/admin/extensions/$extensionId" params={{ extensionId: preview.existingExtensionId }}>View installed</Link></> : null}</p>}
      {valid ? <>
        <Templates templates={preview.templates} note="The stage templates this version offers recipes." />
        <Disclosure summary={<><FileJson size={14} aria-hidden="true" /> Manifest</>}><pre className="code-block">{preview.manifestJson}</pre></Disclosure>
      </> : null}
      <div className="editor-actions">
        <button type="button" className="secondary-button" onClick={() => setStep("source")}><ArrowLeft size={15} aria-hidden="true" /> Back to source</button>
        {valid ? <button type="button" className="primary-button" onClick={() => setStep("permissions")}>Continue to permissions <ArrowRight size={15} aria-hidden="true" /></button> : null}
      </div>
    </> : null}
    {step === "permissions" && valid && preview ? <>
      <Permissions permissions={preview.permissions} chosen={chosen} choose={(id, on) => setChosen((s) => { const next = new Set(s); if (on) next.add(id); else next.delete(id); return next; })} />
      <div className="editor-actions">
        <button type="button" className="secondary-button" onClick={() => setStep("preview")}><ArrowLeft size={15} aria-hidden="true" /> Back</button>
        <button type="button" className="primary-button" onClick={() => setStep("install")}>Continue <ArrowRight size={15} aria-hidden="true" /></button>
      </div>
    </> : null}
    {step === "install" && valid && preview ? <section className="editor-card" aria-labelledby="install-heading">
      <div className="editor-card-heading"><span className="project-symbol"><Download size={18} aria-hidden="true" /></span><div>
        <h2 id="install-heading">Install {preview.extensionKey} v{preview.version}</h2>
        <p>Pinned to commit <code>{short(preview.commit)}</code>. The version is immutable; projects use it only once granted.</p></div></div>
      <SummaryList items={[
        { label: "Approved", value: approved.length ? <span className="mono">{approved.join(", ")}</span> : "None" },
        { label: "Declined", value: preview.permissions.filter((p) => !approved.includes(p.id)).map((p) => p.id).join(", ") || "None" },
        { label: "Templates", value: preview.templates.map((t) => t.id).join(", ") || "None" },
      ]} />
      <div className="editor-actions">
        <button type="button" className="secondary-button" disabled={Boolean(busy)} onClick={() => setStep("permissions")}><ArrowLeft size={15} aria-hidden="true" /> Back</button>
        <button className="primary-button" type="button" disabled={Boolean(busy)} onClick={() => void install()}>
          {busy === "install" ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Download size={15} aria-hidden="true" />}
          {busy === "install" ? "Installing…" : `Install v${preview.version} at ${short(preview.commit)}`}</button>
      </div>
    </section> : null}
  </CreateFlow>;
}

const versionFeatures = tableFeatures({});

// One detail page for Admin › Extensions and Library › Extensions; library
// hides update checks and grants.
export function ExtensionDetailPage({ extensionId, library = false }: { extensionId: string; library?: boolean }) {
  const { org, admin } = useOrg();
  const manage = admin && !library;
  const queryClient = useQueryClient();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const base = useLocation({ select: (l) => l.pathname });
  const detail = useQuery({ queryKey: extensionKey(org, extensionId), enabled: Boolean(org), queryFn: ({ signal }) => getExtension(extensionId, signal) });
  const [selected, setSelected] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const extension = detail.data?.extension;
  const versions = detail.data?.versions || [];
  const version = versions.find((v) => v.id === (selected || extension?.currentVersionId)) || versions[0];
  const invalidate = () => Promise.all([queryClient.invalidateQueries({ queryKey: extensionKey(org, extensionId) }), queryClient.invalidateQueries({ queryKey: ["extensions", org] })]);
  const tabs: TabSpec[] = [
    { id: "overview", label: "Overview" },
    { id: "versions", label: "Versions", count: versions.length },
    { id: "permissions", label: "Permissions", count: version?.permissions.length },
    { id: "templates", label: "Templates", count: version?.templates.length },
    { id: "grants", label: "Grants", count: detail.data?.grants.length, hidden: !manage },
  ];
  const tab = tabFrom(search, tabs);
  const tabLink = (id: string, label: string) => <Link to={base as "/"} search={{ tab: id } as never} className="text-action">{label} <ArrowRight size={13} aria-hidden="true" /></Link>;

  async function checkUpdate() {
    setBusy(true); setError("");
    try { await checkExtensionUpdate(extensionId); await invalidate(); }
    catch (cause) { setError(failure(cause, "The ref could not be resolved. Try again.")); }
    finally { setBusy(false); }
  }

  const columns = useMemo<ColumnDef<typeof versionFeatures, ExtensionVersion>[]>(() => [
    { id: "version", header: "Version", cell: ({ row }) => <button type="button" className="text-action" aria-pressed={row.original.id === version?.id} onClick={() => setSelected(row.original.id)}>
      v{row.original.version}{row.original.id === extension?.currentVersionId ? " · current" : ""}</button> },
    { id: "commit", header: "Commit", cell: ({ row }) => <CopyValue value={row.original.commit} label="Commit" chars={12} /> },
    { id: "manifest", header: "Manifest", cell: ({ row }) => <span><CopyValue value={row.original.manifestSha256} label="Manifest SHA-256" chars={12} /><br />
      <small>{row.original.manifestOrigin === "overlay" ? "Supplied by an admin" : row.original.manifestPath}</small></span> },
    { id: "approved", header: "Approved", cell: ({ row }) => `${row.original.approvedPermissions.length} of ${row.original.permissions.length}` },
    { id: "by", header: "Installed by", cell: ({ row }) => row.original.installedByUsername || "—" },
    { id: "created", header: "Installed", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
  ], [version?.id, extension?.currentVersionId]);
  const table = useTable({ features: versionFeatures, data: versions, columns, getRowId: (row) => row.id });

  if (detail.isPending) return <StatePanel kind="loading" title="Loading extension" />;
  if (detail.isError || !extension) return <StatePanel kind="error" title="Extension unavailable" retry={() => void detail.refetch()}>This extension could not be loaded.</StatePanel>;
  const current = versions.find((v) => v.id === extension.currentVersionId);
  const versionNote = version && versions.length > 1 ? <> Showing v{version.version}; pick another on the <Link to={base as "/"} search={{ tab: "versions" } as never} className="text-action">Versions</Link> tab.</> : null;

  return <DetailLayout back={library ? { href: "/extensions", label: "All extensions" } : { href: "/admin/extensions", label: "All extensions" }}
    title={extension.key} status={<UpdateBadge extension={extension} />}
    facts={[
      { label: "Current version", value: extension.currentVersion ? `v${extension.currentVersion}` : "None" },
      { label: "Commit", value: extension.currentCommit ? <CopyValue value={extension.currentCommit} label="Current commit" chars={12} /> : "—" },
      { label: "Ref", value: <span className="mono">{extension.gitRef}</span> },
      { label: "Checked", value: extension.latestCheckedAt ? <Timestamp value={extension.latestCheckedAt} /> : "Never" },
    ]}
    actions={manage ? <button type="button" className="secondary-button" disabled={busy} onClick={() => void checkUpdate()}>
      <RefreshCw size={14} aria-hidden="true" className={busy ? "spin" : undefined} /> Check for updates</button> : null}
    tabs={tabs} current={tab} tabsLabel="Extension sections">
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    {extension.updateAvailable ? <p className="notice" role="note">{extension.gitRef} now points at <code>{short(extension.latestRefCommit)}</code>; the current version is pinned to <code>{short(extension.currentCommit)}</code>.
      Nothing changes until an admin installs a new version.{manage ? <> <Link className="text-action" to="/admin/extensions/new" search={{ repo: extension.repositoryUrl, ref: extension.gitRef }}>Preview the new commit</Link></> : null}</p> : null}
    {tab === "overview" ? <div className="dash-grid">
      <Card title="About" className="dash-main" description={<span className="mono">{extension.repositoryUrl}</span>}>
        <SummaryList items={[
          { label: "Current", value: current ? <>v{current.version} · installed <Timestamp value={current.createdAt} />{current.installedByUsername ? ` by ${current.installedByUsername}` : ""}</> : "No current version" },
          { label: "Manifest", value: current ? current.manifestOrigin === "overlay" ? "Supplied by an admin" : <span className="mono">{current.manifestPath}</span> : "—" },
          { label: "Permissions", value: current ? `${current.approvedPermissions.length} of ${current.permissions.length} approved` : "—" },
          { label: "Templates", value: current?.templates.length ? current.templates.map((t) => t.reference).join(", ") : "None" },
        ]} />
        <div className="card-body"><Disclosure summary="Advanced: identifiers"><SummaryList items={[
          { label: "Extension ID", value: <CopyValue value={extension.id} label="Extension ID" chars={13} /> },
          ...(current ? [{ label: "Manifest SHA-256", value: <CopyValue value={current.manifestSha256} label="Manifest SHA-256" chars={16} /> },
            { label: "Permissions SHA-256", value: <CopyValue value={current.permissionsSha256} label="Permissions SHA-256" chars={16} /> }] : []),
        ]} /></Disclosure></div>
      </Card>
      <Card title="More" className="dash-side">
        <ul className="link-list">
          <li>{tabLink("versions", `${versions.length} ${versions.length === 1 ? "version" : "versions"}`)}</li>
          <li>{tabLink("permissions", `${version?.permissions.length ?? 0} declared permissions`)}</li>
          <li>{tabLink("templates", `${version?.templates.length ?? 0} stage templates`)}</li>
          {manage ? <li>{tabLink("grants", `${detail.data.grants.length} ${detail.data.grants.length === 1 ? "grant" : "grants"}`)}</li> : null}
        </ul>
        {library ? <p className="card-note">Owners and admins install versions and manage grants.</p> : null}
      </Card>
    </div> : null}
    {tab === "versions" ? <>
      <section className="table-section" aria-labelledby="versions-heading">
        <div className="table-heading"><div><h2 id="versions-heading">Versions</h2><p>Immutable. A version string is bound to one commit; runs record the exact manifest they froze. Select a version to inspect its permissions and templates.</p></div></div>
        <DataTable table={table} label="Extension versions" empty="No versions." />
      </section>
      {version ? <Disclosure summary={<><FileJson size={14} aria-hidden="true" /> Manifest · v{version.version}</>}><pre className="code-block">{version.manifestJson}</pre></Disclosure> : null}
    </> : null}
    {tab === "permissions" && version ? <Permissions permissions={version.permissions} approved={version.approvedPermissions}
      note={<>What v{version.version} was installed with. Runs freeze this list and its digest.{versionNote}</>} /> : null}
    {tab === "templates" && version ? <Templates templates={version.templates}
      note={<>Name a template in a recipe stage as <code>extension@version/template</code>, or pick it in the recipe editor.{versionNote}</>} /> : null}
    {tab === "grants" && manage ? <ResourceGrants grants={detail.data.grants} label="Extension grants" canManage canAdd
      description="Each grant names one project, one user, or a minimum role that may launch recipes using this extension. Owners and admins get no implicit use."
      revokeNote="New runs can no longer use it there; runs already launched keep the version they froze."
      grant={(kind, project, grantee) => grantExtension(extensionId, project, kind, grantee)} revoke={revokeExtensionGrant} onChanged={invalidate} /> : null}
  </DetailLayout>;
}
