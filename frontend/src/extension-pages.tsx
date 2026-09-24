import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, Check, Download, FileJson, Package, Plus, RefreshCw, Search } from "lucide-react";
import { isOrgAdmin } from "./admin";
import { currentSession, sessionQueryKey } from "./auth";
import { failure, ResourceGrants } from "./connection-ui";
import { DataTable } from "./data-table";
import { TextField } from "./form-field";
import type { Extension, ExtensionPermission, ExtensionTemplate, ExtensionVersion, PreviewExtensionInstallResponse } from "./gen/blaxsmith/api/v1/extensions_pb";
import { PageHeader, PageShell } from "./page";
import {
  approvedPermissions, checkExtensionUpdate, extensionKey, extensionsKey, getExtension, grantExtension, installExtension, listExtensions,
  permissionKinds, previewExtensionInstall, revokeExtensionGrant, type ExtensionSourceInput,
} from "./extensions";

function useOrg() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return { org: session.data?.organizationId || "", admin: isOrgAdmin(session.data) };
}

const short = (commit: string) => commit.slice(0, 12);
const when = (value: string) => value ? <time dateTime={value}>{new Date(value).toLocaleString()}</time> : "—";

const listFeatures = tableFeatures({});

export function ExtensionListPage() {
  const { org, admin } = useOrg();
  const extensions = useQuery({ queryKey: extensionsKey(org), enabled: Boolean(org), queryFn: ({ signal }) => listExtensions(signal) });
  const [search, setSearch] = useState("");
  const rows = useMemo(() => (extensions.data?.extensions || []).filter((e) =>
    !search.trim() || `${e.key} ${e.repositoryUrl}`.toLowerCase().includes(search.trim().toLowerCase())), [extensions.data, search]);
  const columns = useMemo<ColumnDef<typeof listFeatures, Extension>[]>(() => [
    { id: "key", header: "Extension", cell: ({ row }) => <Link to="/admin/extensions/$extensionId" params={{ extensionId: row.original.id }} className="run-link">
      <span className="project-symbol"><Package size={15} aria-hidden="true" /></span>
      <span><strong>{row.original.key}</strong><small className="mono">{row.original.repositoryUrl} @ {row.original.gitRef}</small></span>
      <ArrowRight size={15} aria-hidden="true" /></Link> },
    { id: "current", header: "Current", cell: ({ row }) => row.original.currentVersion
      ? <span>v{row.original.currentVersion}<br /><small className="mono">{short(row.original.currentCommit)}</small></span> : "—" },
    { id: "versions", header: "Versions", cell: ({ row }) => row.original.versionCount },
    { id: "update", header: "Update", cell: ({ row }) => row.original.updateAvailable
      ? <span className="state-badge">Ref moved to {short(row.original.latestRefCommit)}</span> : row.original.latestCheckedAt ? "Up to date" : "Not checked" },
    { id: "checked", header: "Checked", cell: ({ row }) => when(row.original.latestCheckedAt) },
  ], []);
  const table = useTable({ features: listFeatures, data: rows, columns, getRowId: (row) => row.id });
  return <PageShell>
    <PageHeader eyebrow="Administration / Extensions" title="Extensions"
      description="Extension packs pinned to one Git commit. Stage templates run only inside sandboxes, with the permissions approved at install."
      actions={admin ? <Link to="/admin/extensions/new" className="primary-button"><Plus size={15} aria-hidden="true" /> Install extension</Link> : null} />
    <Link to="/admin" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Operations</Link>
    <section className="table-section" aria-labelledby="extensions-heading">
      <div className="table-heading"><div><h2 id="extensions-heading">Installed</h2><p>Versions are immutable. Projects use an extension only through a grant; runs freeze the version, commit, and approved permissions.</p></div>
        <button type="button" className="secondary-button" disabled={extensions.isFetching} onClick={() => void extensions.refetch()}><RefreshCw size={14} aria-hidden="true" className={extensions.isFetching ? "spin" : undefined} /> Refresh</button></div>
      <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search extensions</span>
        <input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search extensions" maxLength={120} /></label></div>
      {extensions.isPending ? <div className="source-summary" role="status">Loading extensions…</div> : null}
      {extensions.isError ? <div className="source-summary" role="alert">Extensions could not be loaded. <button type="button" className="text-action" onClick={() => void extensions.refetch()}>Try again</button></div> : null}
      <DataTable table={table} label="Extensions" empty={extensions.isPending || extensions.isError ? undefined : search ? "No extensions match this search." : "No extensions installed."} />
      <div className="table-footer"><span>{rows.length} shown</span></div>
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

function Templates({ templates }: { templates: ExtensionTemplate[] }) {
  const table = useTable({ features: templateFeatures, data: templates, columns: templateColumns, getRowId: (row) => row.id });
  return <section className="table-section" aria-labelledby="templates-heading">
    <div className="table-heading"><div><h2 id="templates-heading">Stage templates</h2><p>Name a template in a recipe stage as <code>extension@version/template</code>.</p></div></div>
    <DataTable table={table} label="Stage templates" empty="No stage templates." />
  </section>;
}

// Shows declared permissions. With choose, optional ones can be declined; otherwise approved lists what was granted.
function Permissions({ permissions, approved, chosen, choose }: {
  permissions: ExtensionPermission[]; approved?: string[]; chosen?: ReadonlySet<string>; choose?: (id: string, on: boolean) => void;
}) {
  const columns = useMemo<ColumnDef<typeof permissionFeatures, ExtensionPermission>[]>(() => [
    { id: "approve", header: choose ? "Approve" : "Approved", cell: ({ row }) => {
      const p = row.original;
      if (!choose) return approved?.includes(p.id) ? <span className="state-badge"><Check size={12} aria-hidden="true" /> Approved</span> : <span className="state-badge">Declined</span>;
      return <label className="recipe-check"><input type="checkbox" checked={!p.optional || Boolean(chosen?.has(p.id))} disabled={!p.optional}
        onChange={(event) => choose(p.id, event.target.checked)} aria-label={`Approve ${p.id}`} /> {p.optional ? "Optional" : "Required"}</label>;
    } },
    { id: "kind", header: "Kind", cell: ({ row }) => permissionKinds[row.original.kind] || row.original.kind },
    { id: "id", header: "Permission", cell: ({ row }) => <span><code>{row.original.id}</code><br /><small>{row.original.description}</small></span> },
  ], [approved, chosen, choose]);
  const table = useTable({ features: permissionFeatures, data: permissions, columns, getRowId: (row) => row.id });
  return <section className="table-section" aria-labelledby="permissions-heading">
    <div className="table-heading"><div><h2 id="permissions-heading">Permissions</h2><p>{choose
      ? "Everything this extension may do inside a sandbox. Required permissions cannot be declined; declined optional ones never reach a run."
      : "What this version was installed with. Runs freeze this list and its digest."}</p></div></div>
    <DataTable table={table} label="Extension permissions" empty="This extension declares no permissions." />
  </section>;
}

export function ExtensionInstallPage({ initial }: { initial: Partial<ExtensionSourceInput> }) {
  const { org, admin } = useOrg();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [source, setSource] = useState<ExtensionSourceInput>({ repositoryUrl: initial.repositoryUrl || "", gitRef: initial.gitRef || "main",
    manifestPath: initial.manifestPath || "", overlayManifestJson: "" });
  const [preview, setPreview] = useState<PreviewExtensionInstallResponse | null>(null);
  const [previewed, setPreviewed] = useState<ExtensionSourceInput | null>(null);
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const edit = (field: keyof ExtensionSourceInput) => (value: string) => { setSource((s) => ({ ...s, [field]: value })); setPreview(null); };
  const valid = preview && preview.errors.length === 0 && preview.commit;

  async function runPreview() {
    setBusy("preview"); setError("");
    try {
      const result = await previewExtensionInstall(source);
      setPreview(result); setPreviewed(source); setChosen(new Set());
    } catch (cause) {
      setError(failure(cause, "The repository could not be previewed. Check the URL and ref, then try again."));
    } finally { setBusy(""); }
  }

  async function install() {
    if (!preview || !previewed) return;
    setBusy("install"); setError("");
    try {
      const result = await installExtension(previewed, preview.commit, approvedPermissions(preview.permissions, chosen));
      if (result.errors.length) { setPreview({ ...preview, errors: result.errors }); return; }
      await queryClient.invalidateQueries({ queryKey: extensionsKey(org) });
      if (result.extension) await navigate({ to: "/admin/extensions/$extensionId", params: { extensionId: result.extension.id } });
    } catch (cause) {
      setError(failure(cause, "The extension could not be installed. Preview again and retry."));
    } finally { setBusy(""); }
  }

  return <PageShell>
    <PageHeader eyebrow="Administration / Extensions" title="Install extension"
      description="Preview resolves the ref to one commit and validates the manifest there. Install pins that exact commit." />
    <Link to="/admin/extensions" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All extensions</Link>
    {!admin ? <div className="state-panel" role="note"><h2>Installing is restricted</h2><p>Organization owners and admins install extensions.</p></div> : <>
      <div className="recipe-editor"><section className="editor-card" aria-labelledby="source-heading">
        <div className="editor-card-heading"><span className="project-symbol"><Package size={18} aria-hidden="true" /></span><div>
          <h2 id="source-heading">Source</h2><p>A public GitHub or GitLab HTTPS repository. Private repositories are not supported yet.</p></div></div>
        <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); void runPreview(); }}>
          <TextField label="Repository URL" name="repositoryUrl" type="url" autoComplete="off" placeholder="https://github.com/owner/repo" value={source.repositoryUrl} onChange={edit("repositoryUrl")} onBlur={() => undefined} />
          <TextField label="Git ref" name="gitRef" autoComplete="off" placeholder="main, a tag, or a commit" value={source.gitRef} onChange={edit("gitRef")} onBlur={() => undefined} />
          <TextField label="Manifest path" name="manifestPath" autoComplete="off" placeholder="blaxsmith-extension.json" value={source.manifestPath} onChange={edit("manifestPath")} onBlur={() => undefined} required={false} />
          <details className="recipe-advanced" open={Boolean(source.overlayManifestJson)}><summary><FileJson size={14} aria-hidden="true" /> Supply the manifest (for a repository without one)</summary>
            <label className="recipe-json form-field"><span>Overlay manifest JSON</span>
              <textarea spellCheck={false} rows={16} value={source.overlayManifestJson} onChange={(event) => edit("overlayManifestJson")(event.target.value)} /></label></details>
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
          <div className="editor-actions">
            <Link to="/admin/extensions" className="secondary-button">Cancel</Link>
            <button className="secondary-button" type="submit" disabled={Boolean(busy) || !source.repositoryUrl.trim() || !source.gitRef.trim()}>
              <RefreshCw size={15} aria-hidden="true" className={busy === "preview" ? "spin" : undefined} /> {busy === "preview" ? "Previewing…" : "Preview"}</button>
          </div>
        </form>
      </section></div>
      {preview && preview.errors.length ? <div className="recipe-validation is-invalid" role="alert">
        {preview.errors.map((e) => <span key={`${e.path}:${e.message}`}><code>{e.path}</code> {e.message}</span>)}</div> : null}
      {valid ? <>
        <p className="notice" role="status"><strong>{preview.extensionKey} v{preview.version}</strong> at commit <code>{preview.commit}</code> · manifest sha256 <code>{short(preview.manifestSha256)}</code>
          {preview.existingExtensionId ? <> · already installed; this adds a version. <Link className="text-action" to="/admin/extensions/$extensionId" params={{ extensionId: preview.existingExtensionId }}>View installed</Link></> : null}</p>
        <Permissions permissions={preview.permissions} chosen={chosen} choose={(id, on) => setChosen((s) => { const next = new Set(s); if (on) next.add(id); else next.delete(id); return next; })} />
        <Templates templates={preview.templates} />
        <details className="recipe-source"><summary><FileJson size={14} aria-hidden="true" /> Manifest</summary><pre className="mono">{preview.manifestJson}</pre></details>
        <div className="editor-actions">
          <button className="primary-button" type="button" disabled={Boolean(busy)} onClick={() => void install()}>
            {busy === "install" ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Download size={15} aria-hidden="true" />}
            {busy === "install" ? "Installing…" : `Install v${preview.version} at ${short(preview.commit)}`}</button>
        </div>
      </> : null}
    </>}
  </PageShell>;
}

const versionFeatures = tableFeatures({});

export function ExtensionDetailPage({ extensionId }: { extensionId: string }) {
  const { org, admin } = useOrg();
  const queryClient = useQueryClient();
  const detail = useQuery({ queryKey: extensionKey(org, extensionId), enabled: Boolean(org), queryFn: ({ signal }) => getExtension(extensionId, signal) });
  const [selected, setSelected] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const extension = detail.data?.extension;
  const versions = detail.data?.versions || [];
  const version = versions.find((v) => v.id === (selected || extension?.currentVersionId)) || versions[0];
  const invalidate = () => Promise.all([queryClient.invalidateQueries({ queryKey: extensionKey(org, extensionId) }), queryClient.invalidateQueries({ queryKey: extensionsKey(org) })]);

  async function checkUpdate() {
    setBusy(true); setError("");
    try { await checkExtensionUpdate(extensionId); await invalidate(); }
    catch (cause) { setError(failure(cause, "The ref could not be resolved. Try again.")); }
    finally { setBusy(false); }
  }

  const columns = useMemo<ColumnDef<typeof versionFeatures, ExtensionVersion>[]>(() => [
    { id: "version", header: "Version", cell: ({ row }) => <button type="button" className="text-action" aria-pressed={row.original.id === version?.id} onClick={() => setSelected(row.original.id)}>
      v{row.original.version}{row.original.id === extension?.currentVersionId ? " · current" : ""}</button> },
    { id: "commit", header: "Commit", cell: ({ row }) => <code title={row.original.commit}>{short(row.original.commit)}</code> },
    { id: "manifest", header: "Manifest", cell: ({ row }) => <span><code title={row.original.manifestSha256}>{short(row.original.manifestSha256)}</code><br />
      <small>{row.original.manifestOrigin === "overlay" ? "Supplied by an admin" : row.original.manifestPath}</small></span> },
    { id: "approved", header: "Approved", cell: ({ row }) => `${row.original.approvedPermissions.length} of ${row.original.permissions.length}` },
    { id: "by", header: "Installed by", cell: ({ row }) => row.original.installedByUsername || "—" },
    { id: "created", header: "Installed", cell: ({ row }) => when(row.original.createdAt) },
  ], [version?.id, extension?.currentVersionId]);
  const table = useTable({ features: versionFeatures, data: versions, columns, getRowId: (row) => row.id });

  return <PageShell>
    <PageHeader eyebrow="Administration / Extensions" title={extension?.key || "Extension"}
      description={extension ? `${extension.repositoryUrl} @ ${extension.gitRef}` : undefined}
      actions={admin && extension ? <button type="button" className="secondary-button" disabled={busy} onClick={() => void checkUpdate()}>
        <RefreshCw size={14} aria-hidden="true" className={busy ? "spin" : undefined} /> Check for updates</button> : null} />
    <Link to="/admin/extensions" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All extensions</Link>
    {detail.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading extension</h2></div> : null}
    {detail.isError ? <div className="state-panel" role="alert"><h2>Extension unavailable</h2><p>This extension could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void detail.refetch()}>Try again</button></div> : null}
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    {extension?.updateAvailable ? <p className="notice" role="note">{extension.gitRef} now points at <code>{short(extension.latestRefCommit)}</code>; the current version is pinned to <code>{short(extension.currentCommit)}</code>.
      Nothing changes until an admin installs a new version.{admin ? <> <Link className="text-action" to="/admin/extensions/new" search={{ repo: extension.repositoryUrl, ref: extension.gitRef }}>Preview the new commit</Link></> : null}</p> : null}
    {extension ? <section className="table-section" aria-labelledby="versions-heading">
      <div className="table-heading"><div><h2 id="versions-heading">Versions</h2><p>Immutable. A version string is bound to one commit; runs record the exact manifest they froze.</p></div></div>
      <DataTable table={table} label="Extension versions" empty="No versions." />
    </section> : null}
    {extension && admin ? <ResourceGrants grants={detail.data?.grants || []} label="Extension grants" canManage canAdd
      description="Each grant names one project, one user, or a minimum role that may launch recipes using this extension. Owners and admins get no implicit use."
      revokeNote="New runs can no longer use it there; runs already launched keep the version they froze."
      grant={(kind, project, grantee) => grantExtension(extensionId, project, kind, grantee)} revoke={revokeExtensionGrant} onChanged={invalidate} /> : null}
    {version ? <>
      <Permissions permissions={version.permissions} approved={version.approvedPermissions} />
      <Templates templates={version.templates} />
      <details className="recipe-source"><summary><FileJson size={14} aria-hidden="true" /> Manifest · v{version.version}</summary><pre className="mono">{version.manifestJson}</pre></details>
    </> : null}
  </PageShell>;
}
