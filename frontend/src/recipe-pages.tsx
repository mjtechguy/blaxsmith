import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, BookCopy, Check, CopyPlus, FileJson, GitFork, ListTree, Package, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
import { extensionKey, extensionsKey, getExtension, listExtensions, templateChoices } from "./extensions";
import type { ExtensionVersion } from "./gen/blaxsmith/api/v1/extensions_pb";
import { currentSession, sessionQueryKey } from "./auth";
import { ResourceGrants, useConnectionModels } from "./connection-ui";
import { ModelSelect } from "./model-select";
import { effortChoices, effortForModel } from "./connections";
import { DataTable } from "./data-table";
import { TextField } from "./form-field";
import type { LibraryRecipe, RecipeModelConnection, RecipeValidationError, RecipeVersion } from "./gen/blaxsmith/api/v1/recipes_pb";
import { PageHeader, PageShell } from "./page";
import { CollectionTable, inSet, useUrlView, type GridColumn } from "./data-table";
import { DetailLayout, SummaryList } from "./layouts";
import { Card, CopyValue, Disclosure, EmptyState, StatePanel, tabFrom, Timestamp, type TabSpec } from "./ui";
import {
  cloneRecipe, createRecipe, createRecipeVersion, emptyRecipe, formatRecipe, getRecipe, getRecipeEditorOptions, grantRecipe, revokeRecipeGrant,
  getRecipeVersion, listProjectRecipeFiles, listRecipes, mayEditRecipes, parseRecipe, recipeFilesKey, recipeKey, recipeOptionsKey,
  recipesKey, recipeVersionKey, renameProfile, renameStage, setCurrentRecipeVersion, stageRows, validateRecipe,
  type RecipeDocument, type RecipeProfile, type RecipeStage, type StageRow,
} from "./recipes";
import { listTools } from "./tools-page";

type Page = "list" | "new" | "detail" | "version";

// One link vocabulary for the organization (Library) and project recipe routes.
export function RecipeLink({ projectId, recipeId = "", page, from, className = "text-action", children }: {
  projectId?: string; recipeId?: string; page: Page; from?: string; className?: string; children: ReactNode;
}) {
  const search = from ? { from } : {};
  if (projectId) {
    if (page === "list") return <Link className={className} to="/projects/$projectId/recipes" params={{ projectId }}>{children}</Link>;
    if (page === "new") return <Link className={className} to="/projects/$projectId/recipes/new" params={{ projectId }} search={search}>{children}</Link>;
    if (page === "detail") return <Link className={className} to="/projects/$projectId/recipes/$recipeId" params={{ projectId, recipeId }}>{children}</Link>;
    return <Link className={className} to="/projects/$projectId/recipes/$recipeId/versions/new" params={{ projectId, recipeId }} search={search}>{children}</Link>;
  }
  // Organization recipes: one set of Library pages; manage actions show by role.
  if (page === "list") return <Link className={className} to="/recipes">{children}</Link>;
  if (page === "new") return <Link className={className} to="/recipes/new" search={search}>{children}</Link>;
  if (page === "detail") return <Link className={className} to="/recipes/$recipeId" params={{ recipeId }}>{children}</Link>;
  return <Link className={className} to="/recipes/$recipeId/versions/new" params={{ recipeId }} search={search}>{children}</Link>;
}

function useSession() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return { session, org: session.data?.organizationId || "", mayEdit: mayEditRecipes(session.data) };
}

const listFeatures = tableFeatures({});

export function RecipeLibraryPage({ projectId }: { projectId?: string }) {
  const { org, mayEdit } = useSession();
  const recipes = useQuery({ queryKey: recipesKey(org, projectId), enabled: Boolean(org), queryFn: ({ signal }) => listRecipes(projectId, signal) });
  const [view, setView] = useUrlView({ sort: [{ id: "name", desc: false }], size: 20 });
  const columns = useMemo<GridColumn<LibraryRecipe>[]>(() => [
    { id: "name", accessorKey: "name", header: "Recipe", enableHiding: false, cell: ({ row }) => <RecipeLink projectId={projectId} recipeId={row.original.id} page="detail" className="run-link">
      <span className="project-symbol"><BookCopy size={15} aria-hidden="true" /></span>
      <span><strong>{row.original.name}</strong><small>{row.original.description || "No description"}</small></span></RecipeLink> },
    { id: "scope", accessorFn: (r) => r.projectId ? "project" : "organization", header: "Scope", filterFn: inSet, cell: ({ row }) => <span className="state-badge">{row.original.projectId ? "Project" : projectId ? "Organization · granted" : "Organization"}</span> },
    { id: "current", accessorKey: "currentVersion", header: "Current", cell: ({ row }) => row.original.currentVersion ? `v${row.original.currentVersion} of ${row.original.versionCount}` : "—" },
    { id: "updated", accessorKey: "updatedAt", header: "Updated", cell: ({ row }) => <Timestamp value={row.original.updatedAt} /> },
    { id: "actions", header: "Actions", enableSorting: false, enableHiding: false, cell: ({ row }) => mayEdit && row.original.currentVersionId
      ? <RecipeLink projectId={projectId} page="new" from={row.original.currentVersionId}><GitFork size={14} aria-hidden="true" /> {projectId && !row.original.projectId ? "Clone to project" : "Clone"}</RecipeLink> : null },
  ], [projectId, mayEdit]);
  const create = mayEdit ? <RecipeLink projectId={projectId} page="new" className="primary-button"><Plus size={15} aria-hidden="true" /> New recipe</RecipeLink> : null;

  return <PageShell>
    <PageHeader title={projectId ? "Project recipes" : "Recipes"}
      description={projectId ? "This project's recipes plus organization recipes granted to it. Runs freeze an exact version." : "Organization recipes: versioned stage graphs, role profiles, checks, and limits. Projects use the ones granted to them."}
      actions={create} />
    <section className="table-section" aria-label="Recipes">
      <CollectionTable id={projectId ? "project-recipes" : "recipes"} label="Recipes" noun="recipes" columns={columns} data={recipes.data?.recipes ?? []} getRowId={(r) => r.id}
        view={view} onView={setView} pinFirst searchLabel="Search recipes"
        facets={projectId ? [{ id: "scope", label: "Scope", options: [{ value: "project", label: "Project" }, { value: "organization", label: "Organization" }] }] : []}
        loading={recipes.isPending} refreshing={recipes.isFetching && !recipes.isPending}
        error={recipes.isError ? <>Recipes could not be loaded. <button type="button" className="text-action" onClick={() => void recipes.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState icon={<BookCopy size={22} aria-hidden="true" />} title="No recipes yet" action={create}>{mayEdit ? "Create a recipe or clone one to start." : "Owners and admins create recipes; members read and use them."}</EmptyState>} />
    </section>
  </PageShell>;
}

const versionFeatures = tableFeatures({});

export function RecipeDetailPage({ projectId, recipeId }: { projectId?: string; recipeId: string }) {
  const { org, mayEdit } = useSession();
  const queryClient = useQueryClient();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const recipe = useQuery({ queryKey: recipeKey(org, recipeId), enabled: Boolean(org), queryFn: ({ signal }) => getRecipe(recipeId, signal) });
  const [selected, setSelected] = useState("");
  const versionId = selected || recipe.data?.recipe?.currentVersionId || recipe.data?.versions[0]?.id || "";
  const version = useQuery({ queryKey: recipeVersionKey(org, versionId), enabled: Boolean(org && versionId), queryFn: ({ signal }) => getRecipeVersion(versionId, signal) });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const current = recipe.data?.recipe;
  const editable = mayEdit && Boolean(current) && (projectId ? current?.projectId === projectId : !current?.projectId);
  const showGrants = Boolean(current && !projectId && !current.projectId && mayEdit);
  const tabs: TabSpec[] = [
    { id: "overview", label: "Overview" },
    { id: "versions", label: "Versions", count: recipe.data?.versions.length },
    { id: "grants", label: "Grants", count: recipe.data?.grants.length, hidden: !showGrants },
  ];
  const tab = tabFrom(search, tabs);

  async function markCurrent(id: string) {
    setBusy(id);
    setError("");
    try {
      await setCurrentRecipeVersion(recipeId, id);
      await Promise.all([queryClient.invalidateQueries({ queryKey: recipeKey(org, recipeId) }), queryClient.invalidateQueries({ queryKey: ["recipes", org] })]);
    } catch (cause) {
      setError(ConnectError.from(cause).code === Code.PermissionDenied ? "Your session cannot change this recipe." : "The current version could not be changed.");
    } finally {
      setBusy("");
    }
  }

  const columns = useMemo<ColumnDef<typeof versionFeatures, RecipeVersion>[]>(() => [
    { id: "version", header: "Version", cell: ({ row }) => <button type="button" className="text-action" aria-pressed={row.original.id === versionId} onClick={() => setSelected(row.original.id)}>
      v{row.original.version}{row.original.id === current?.currentVersionId ? " · current" : ""}</button> },
    { id: "sha", header: "SHA-256", cell: ({ row }) => <CopyValue value={row.original.sha256} label="Version SHA-256" chars={12} /> },
    { id: "path", header: "Frozen as", cell: ({ row }) => <span className="mono">{row.original.frozenPath}</span> },
    { id: "author", header: "Author", cell: ({ row }) => row.original.authorUsername || <span className="state-badge">seed</span> },
    { id: "created", header: "Created", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
    { id: "actions", header: "Actions", cell: ({ row }) => <span className="recipe-actions">
      {editable && row.original.id !== current?.currentVersionId ? <button type="button" className="text-action" disabled={Boolean(busy)} onClick={() => void markCurrent(row.original.id)}><Check size={14} aria-hidden="true" /> Mark current</button> : null}
      {editable ? <RecipeLink projectId={projectId} recipeId={recipeId} page="version" from={row.original.id}><CopyPlus size={14} aria-hidden="true" /> New version from this</RecipeLink> : null}
      {mayEdit ? <RecipeLink projectId={projectId} page="new" from={row.original.id}><GitFork size={14} aria-hidden="true" /> Clone</RecipeLink> : null}
    </span> },
  ], [versionId, current?.currentVersionId, editable, mayEdit, busy, projectId, recipeId]);
  const table = useTable({ features: versionFeatures, data: recipe.data?.versions || [], columns, getRowId: (row) => row.id });
  const doc = parseRecipe(version.data?.version?.recipeJson || "");
  const validation = useQuery({ queryKey: ["recipe-order", org, versionId], enabled: Boolean(org && version.data?.version),
    queryFn: ({ signal }) => validateRecipe(version.data?.version?.recipeJson || "", "", signal) });
  const base = useLocation({ select: (l) => l.pathname });
  const tabLink = (id: string, label: string) => <Link to={base as "/"} search={{ tab: id } as never} className="text-action">{label} <ArrowRight size={13} aria-hidden="true" /></Link>;

  if (recipe.isPending) return <StatePanel kind="loading" title="Loading recipe" />;
  if (recipe.isError || !current) return <StatePanel kind="error" title="Recipe unavailable" retry={() => void recipe.refetch()}>This recipe could not be loaded.</StatePanel>;
  const currentVersion = recipe.data.versions.find((v) => v.id === current.currentVersionId);
  const listHref = projectId ? `/projects/${projectId}/recipes` : "/recipes";

  return <DetailLayout back={{ href: listHref, label: "All recipes" }} title={current.name}
    status={<span className="state-badge">{current.projectId ? "Project recipe" : "Organization recipe"}</span>}
    facts={[
      { label: "Current version", value: current.currentVersion ? `v${current.currentVersion} of ${current.versionCount}` : "None" },
      { label: "Updated", value: <Timestamp value={current.updatedAt} /> },
      { label: "Stages", value: doc ? doc.stages.length : "—" },
    ]}
    actions={editable ? <RecipeLink projectId={projectId} recipeId={recipeId} page="version" from={versionId} className="primary-button"><Plus size={15} aria-hidden="true" /> New version</RecipeLink> : null}
    tabs={tabs} current={tab} tabsLabel="Recipe sections">
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    {tab === "overview" ? <>
      <div className="dash-grid">
        <Card title="About" className="dash-main" description={current.description || "No description."}>
          <SummaryList items={[
            { label: "Scope", value: current.projectId ? "This project only" : projectId ? "Organization, granted to this project" : "Organization" },
            { label: "Current", value: currentVersion ? <>v{currentVersion.version} · frozen as <span className="mono">{currentVersion.frozenPath}</span></> : "No current version" },
            { label: "Order", value: validation.data?.stageOrder.length ? validation.data.stageOrder.join(" → ") : "—" },
          ]} />
          <div className="card-body"><Disclosure summary="Advanced: identifiers"><SummaryList items={[
            { label: "Recipe ID", value: <CopyValue value={current.id} label="Recipe ID" chars={13} /> },
            ...(currentVersion ? [{ label: "SHA-256", value: <CopyValue value={currentVersion.sha256} label="Version SHA-256" chars={16} /> }] : []),
          ]} /></Disclosure></div>
        </Card>
        <Card title="More" className="dash-side">
          <ul className="link-list">
            <li>{tabLink("versions", `${recipe.data.versions.length} ${recipe.data.versions.length === 1 ? "version" : "versions"}`)}</li>
            {showGrants ? <li>{tabLink("grants", `${recipe.data.grants.length} ${recipe.data.grants.length === 1 ? "grant" : "grants"}`)}</li> : null}
          </ul>
        </Card>
      </div>
      {version.data?.version ? <StageDag doc={doc} order={validation.data?.stageOrder} title={`Stage graph · v${version.data.version.version}`} /> : null}
    </> : null}
    {tab === "versions" ? <>
      <section className="table-section" aria-labelledby="versions-heading">
        <div className="table-heading"><div><h2 id="versions-heading">Versions</h2><p>Immutable. Runs record the exact bytes they froze; changing the current version affects only new runs. Select a version to inspect it.</p></div></div>
        <DataTable table={table} label="Recipe versions" empty="No versions." />
      </section>
      {version.data?.version ? <StageDag doc={doc} order={validation.data?.stageOrder} title={`Stage graph · v${version.data.version.version}`} /> : null}
      {version.data?.version ? <Disclosure summary={<><FileJson size={14} aria-hidden="true" /> Advanced: raw JSON · v{version.data.version.version}</>}><pre className="code-block">{version.data.version.recipeJson}</pre></Disclosure> : null}
    </> : null}
    {tab === "grants" && showGrants ? <ResourceGrants grants={recipe.data.grants} label="Recipe grants" canManage canAdd
      description="Each grant names one project, one user, or a minimum role that may use this recipe from any project. Owners and admins get no implicit use; viewers never. Project recipes need no grant."
      revokeNote="New runs can no longer launch it there; runs already launched keep the bytes they froze."
      grant={(kind, project, grantee) => grantRecipe(recipeId, project, kind, grantee)} revoke={revokeRecipeGrant}
      explain={{ kind: "recipe", resourceId: recipeId }}
      onChanged={() => Promise.all([queryClient.invalidateQueries({ queryKey: recipeKey(org, recipeId) }), queryClient.invalidateQueries({ queryKey: ["recipes", org] })])} /> : null}
  </DetailLayout>;
}

const dagFeatures = tableFeatures({});
const dagColumns: ColumnDef<typeof dagFeatures, StageRow>[] = [
  { id: "id", header: "Stage", cell: ({ row }) => <span><strong className="mono">{row.original.id}</strong>{row.original.template
    ? <><br /><span className="state-badge extension-badge" title={`Extension stage template ${row.original.template}`}><Package size={11} aria-hidden="true" /> {row.original.template}</span></> : null}</span> },
  { id: "kind", header: "Kind", cell: ({ row }) => <span className="state-badge">{row.original.kind.replaceAll("_", " ")}</span> },
  { id: "profile", header: "Profile", cell: ({ row }) => row.original.profile ? <span>{row.original.profile}<br /><small className="mono">{row.original.harness} · {row.original.model} · {row.original.effort}</small></span> : <span className="state-badge">human</span> },
  { id: "deps", header: "Depends on", cell: ({ row }) => row.original.dependsOn.length ? <span className="mono">{row.original.dependsOn.join(", ")}</span> : "—" },
  { id: "loop", header: "Loop", cell: ({ row }) => row.original.loop || "—" },
];

export function StageDag({ doc, order, title = "Stage graph" }: { doc: RecipeDocument | null; order?: string[]; title?: string }) {
  const rows = useMemo(() => stageRows(doc, order), [doc, order]);
  const table = useTable({ features: dagFeatures, data: rows, columns: dagColumns, getRowId: (row, index) => `${row.id}-${index}` });
  return <section className="table-section" aria-labelledby="dag-heading">
    <div className="table-heading"><div><h2 id="dag-heading"><ListTree size={15} aria-hidden="true" /> {title}</h2><p>{order?.length ? "In dependency order." : "In declaration order; the graph is ordered once it validates."}</p></div></div>
    <DataTable table={table} label="Recipe stages" empty={doc ? "No stages." : "Fix the JSON to show stages."} />
  </section>;
}

// Create, clone (from), or new version (recipeId) pages share one editor.
export function RecipeEditorPage({ projectId, recipeId, from }: { projectId?: string; recipeId?: string; from?: string }) {
  const { org, mayEdit, session } = useSession();
  const target = useQuery({ queryKey: recipeKey(org, recipeId || ""), enabled: Boolean(org && recipeId), queryFn: ({ signal }) => getRecipe(recipeId || "", signal) });
  // A new version starts from the chosen version, else the current one.
  const fromId = from || target.data?.recipe?.currentVersionId || "";
  const source = useQuery({ queryKey: recipeVersionKey(org, fromId), enabled: Boolean(org && fromId), queryFn: ({ signal }) => getRecipeVersion(fromId, signal) });
  const sourceRecipe = useQuery({ queryKey: recipeKey(org, source.data?.version?.recipeId || ""), enabled: Boolean(org && !recipeId && source.data?.version),
    queryFn: ({ signal }) => getRecipe(source.data?.version?.recipeId || "", signal) });
  const title = recipeId ? `New version · ${target.data?.recipe?.name || "recipe"}` : from ? "Clone recipe" : "New recipe";
  const ready = session.isSuccess && (!recipeId || target.isSuccess) && (!fromId || source.isSuccess) && (recipeId || !fromId || !sourceRecipe.isPending);
  return <PageShell>
    <PageHeader title={title}
      description="Edit with the form or JSON; both write the same document. The server validates as you type." />
    {recipeId ? <RecipeLink projectId={projectId} recipeId={recipeId} page="detail"><ArrowLeft size={15} aria-hidden="true" /> Back to recipe</RecipeLink>
      : <RecipeLink projectId={projectId} page="list"><ArrowLeft size={15} aria-hidden="true" /> All recipes</RecipeLink>}
    {session.data && !mayEdit ? <div className="state-panel" role="note"><h2>Recipes are read-only</h2><p>Organization owners and admins edit recipes.</p></div> : null}
    {source.isError || target.isError ? <div className="state-panel" role="alert"><h2>Recipe unavailable</h2><p>The source recipe could not be loaded.</p></div> : null}
    {!ready && !source.isError && !target.isError ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading editor</h2></div> : null}
    {ready && mayEdit ? <RecipeEditor org={org} projectId={projectId} recipeId={recipeId} source={source.data?.version}
      baseName={target.data?.recipe?.name || sourceRecipe.data?.recipe?.name} /> : null}
  </PageShell>;
}

function RecipeEditor({ org, projectId, recipeId, source, baseName }: { org: string; projectId?: string; recipeId?: string; source?: RecipeVersion; baseName?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const initialJson = source?.recipeJson || formatRecipe(emptyRecipe());
  const [json, setJson] = useState(initialJson);
  const [frozenPath, setFrozenPath] = useState(source?.frozenPath || "");
  const [name, setName] = useState(source && !recipeId ? `${baseName || "Recipe"} copy` : "");
  const [description, setDescription] = useState("");
  const [makeCurrent, setMakeCurrent] = useState(true);
  const [view, setView] = useState<"form" | "json">("form");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [debounced, setDebounced] = useState({ json, frozenPath });
  useEffect(() => { const timer = window.setTimeout(() => setDebounced({ json, frozenPath }), 350); return () => window.clearTimeout(timer); }, [json, frozenPath]);
  const validation = useQuery({ queryKey: ["recipe-validate", org, debounced.json, debounced.frozenPath], enabled: Boolean(org),
    queryFn: ({ signal }) => validateRecipe(debounced.json, debounced.frozenPath, signal), placeholderData: (previous) => previous });
  const options = useQuery({ queryKey: recipeOptionsKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getRecipeEditorOptions(signal), staleTime: 60_000 });
  const tools = useQuery({ queryKey: ["tool-catalog"], queryFn: ({ signal }) => listTools(signal), staleTime: 5 * 60_000, retry: false });
  const files = useQuery({ queryKey: recipeFilesKey(org, projectId || ""), enabled: Boolean(org && projectId), queryFn: ({ signal }) => listProjectRecipeFiles(projectId || "", signal), staleTime: 60_000, retry: false });
  // Extension templates a stage may name: those granted to this project, or
  // for an organization recipe every installed one (launch still needs a grant).
  const extensions = useQuery({ queryKey: extensionsKey(org, projectId || ""), enabled: Boolean(org), queryFn: ({ signal }) => listExtensions(projectId || "", signal), staleTime: 60_000, retry: false });
  const extensionDetails = useQueries({ queries: (extensions.data?.extensions ?? []).map((e) => ({
    queryKey: extensionKey(org, e.id), queryFn: ({ signal }: { signal: AbortSignal }) => getExtension(e.id, signal), staleTime: 60_000 })) });
  const extensionSets: ExtensionSet[] = (extensions.data?.extensions ?? []).map((e, i) => ({ key: e.key, currentVersion: e.currentVersion, versions: extensionDetails[i]?.data?.versions ?? [] }));
  const doc = parseRecipe(json);
  const errors: RecipeValidationError[] = validation.data?.errors || [];
  const stale = debounced.json !== json || debounced.frozenPath !== frozenPath || validation.isFetching;
  const update = (next: RecipeDocument) => setJson(formatRecipe(next));

  async function submit() {
    setError("");
    if (!recipeId && !name.trim()) { setError("Enter a library name."); return; }
    setSubmitting(true);
    try {
      let saved: { recipeId: string } | null = null;
      if (recipeId) {
        const response = await createRecipeVersion(recipeId, json, frozenPath.trim(), makeCurrent);
        if (response.errors.length) { setError(`Invalid at ${response.errors[0].path}: ${response.errors[0].message}`); return; }
        saved = { recipeId };
      } else if (source && json === source.recipeJson && frozenPath === source.frozenPath) {
        const response = await cloneRecipe(source.id, projectId || "", name.trim(), description.trim());
        saved = { recipeId: response.recipe?.id || "" };
      } else {
        const response = await createRecipe(projectId || "", name.trim(), description.trim(), json, frozenPath.trim());
        if (response.errors.length) { setError(`Invalid at ${response.errors[0].path}: ${response.errors[0].message}`); return; }
        saved = { recipeId: response.recipe?.id || "" };
      }
      await queryClient.invalidateQueries({ queryKey: ["recipes", org] });
      await queryClient.invalidateQueries({ queryKey: recipeKey(org, saved.recipeId) });
      if (projectId) await navigate({ to: "/projects/$projectId/recipes/$recipeId", params: { projectId, recipeId: saved.recipeId } });
      else await navigate({ to: "/recipes/$recipeId", params: { recipeId: saved.recipeId } });
    } catch (cause) {
      const code = ConnectError.from(cause).code;
      setError(code === Code.AlreadyExists ? "A recipe with this name already exists in this scope."
        : code === Code.PermissionDenied ? "Your session cannot change recipes here."
          : code === Code.NotFound ? "The source recipe is not available to this project."
            : code === Code.InvalidArgument ? ConnectError.from(cause).rawMessage : "The recipe could not be saved. Please try again.");
    } finally {
      setSubmitting(false);
    }
  }

  return <div className="recipe-editor">
    <section className="editor-card" aria-labelledby="recipe-editor-heading">
      <div className="editor-card-heading"><span className="project-symbol"><BookCopy size={18} aria-hidden="true" /></span><div>
        <h2 id="recipe-editor-heading">{recipeId ? "New immutable version" : "Recipe"}</h2>
        <p>{projectId ? "Project recipe. Prompt and skill paths resolve against this project's repository at launch." : "Organization recipe. Paths resolve against each launching project's repository."}</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); void submit(); }}>
        {!recipeId ? <>
          <TextField label="Library name" name="name" autoComplete="off" placeholder="Guild engineering (fast)" value={name} onChange={setName} onBlur={() => undefined} />
          <TextField label="Description" name="description" autoComplete="off" placeholder="What this recipe is for" value={description} onChange={setDescription} onBlur={() => undefined} required={false} />
        </> : null}
        <TextField label="Frozen path label" name="frozenPath" autoComplete="off" placeholder=".blaxsmith/recipes/<recipe name>.json" value={frozenPath} onChange={setFrozenPath} onBlur={() => undefined} required={false}
          error={errors.find((e) => e.path === "frozen_path")?.message} />
        <div className="recipe-tabs" role="tablist" aria-label="Editor view">
          <button type="button" role="tab" aria-selected={view === "form"} className={view === "form" ? "is-active" : undefined} onClick={() => setView("form")}><ListTree size={14} aria-hidden="true" /> Form</button>
          <button type="button" role="tab" aria-selected={view === "json"} className={view === "json" ? "is-active" : undefined} onClick={() => setView("json")}><FileJson size={14} aria-hidden="true" /> JSON</button>
        </div>
        {view === "json" || !doc ? <label className="recipe-json form-field"><span>Recipe JSON</span>
          <textarea spellCheck={false} value={json} onChange={(event) => setJson(event.target.value)} aria-invalid={errors.length ? true : undefined} aria-describedby="recipe-validation" rows={28} /></label> : null}
        {view === "form" && !doc ? <p className="notice" role="note">The JSON does not parse into a recipe document. Fix it here to return to the form.</p> : null}
        {view === "form" && doc ? <RecipeForm doc={doc} update={update} errors={errors}
          harnesses={options.data?.harnesses.filter((h) => !tools.data || tools.data.tools.some((tool) => tool.tool === h.harness)) || []}
          allHarnesses={options.data?.harnesses || []} stageKinds={options.data?.stageKinds || []} connections={options.data?.connections || []}
          skillPaths={files.data?.skillPaths} promptPaths={files.data?.promptPaths} extensions={extensionSets} orgRecipe={!projectId} /> : null}
        <div id="recipe-validation" className={errors.length ? "recipe-validation is-invalid" : "recipe-validation"} role="status" aria-live="polite">
          {stale ? <span><RefreshCw size={13} className="spin" aria-hidden="true" /> Validating…</span>
            : validation.isError ? <span>Validation is unavailable.</span>
              : errors.length ? errors.map((e) => <span key={`${e.path}:${e.message}`}><code>{e.path}</code> {e.message}</span>)
                : <span><Check size={13} aria-hidden="true" /> Valid · order {validation.data?.stageOrder.join(" → ")}</span>}
        </div>
        {recipeId ? <label className="recipe-check"><input type="checkbox" checked={makeCurrent} onChange={(event) => setMakeCurrent(event.target.checked)} /> Mark this version current</label> : null}
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions">
          {recipeId ? <RecipeLink projectId={projectId} recipeId={recipeId} page="detail" className="secondary-button">Cancel</RecipeLink> : <RecipeLink projectId={projectId} page="list" className="secondary-button">Cancel</RecipeLink>}
          <button className="primary-button" type="submit" disabled={submitting || stale || errors.length > 0}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Saving…" : recipeId ? "Create version" : "Create recipe"}</button>
        </div>
      </form>
    </section>
    <StageDag doc={doc} order={errors.length ? undefined : validation.data?.stageOrder} />
    {projectId && files.isError ? <p className="notice" role="note">Repository files could not be listed; enter skill and prompt paths directly.</p> : null}
    {projectId && files.data ? <p className="notice" role="note">Skill and prompt pickers list files at commit <code>{files.data.commit.slice(0, 12)}</code>. Launch freezes them at the commit current then.</p> : null}
  </div>;
}

function Select({ label, value, choices, onChange, error }: { label: string; value: string; choices: Array<[string, string]>; onChange: (value: string) => void; error?: string }) {
  const known = choices.some(([choice]) => choice === value);
  return <label className="form-field"><span>{label}</span>
    <select value={value} onChange={(event) => onChange(event.target.value)} aria-invalid={error ? true : undefined}>
      {!known ? <option value={value}>{value ? `${value} (current)` : "Choose…"}</option> : null}
      {choices.map(([choice, text]) => <option key={choice} value={choice}>{text}</option>)}
    </select>
    {error ? <span className="form-field-error">{error}</span> : null}</label>;
}

function fieldError(errors: RecipeValidationError[], prefix: string) {
  return errors.find((e) => e.path === prefix || e.path.startsWith(`${prefix}.`) || e.path.startsWith(`${prefix}[`))?.message;
}

type FormProps = {
  doc: RecipeDocument; update: (doc: RecipeDocument) => void; errors: RecipeValidationError[];
  harnesses: Array<{ harness: string; provider: string; efforts: string[] }>; allHarnesses: Array<{ harness: string; provider: string; efforts: string[] }>;
  stageKinds: string[]; connections: RecipeModelConnection[]; skillPaths?: string[]; promptPaths?: string[];
  extensions: ExtensionSet[]; orgRecipe: boolean;
};

type ExtensionSet = { key: string; currentVersion: string; versions: ExtensionVersion[] };

const list = (value: string) => value.split(",").map((part) => part.trim()).filter(Boolean);
const int = (value: string) => Number.parseInt(value, 10) || 0;

function RecipeForm(props: FormProps) {
  const { doc, update, errors } = props;
  return <>
    <TextField label="Recipe ID" name="recipe-name" autoComplete="off" placeholder="guild-engineering" value={doc.name || ""} onChange={(name) => update({ ...doc, name })} onBlur={() => undefined} error={fieldError(errors, "name")} />
    <TextField label="Required checks (comma separated)" name="required-checks" autoComplete="off" placeholder="project-tests, requirement-coverage" value={(doc.required_checks || []).join(", ")}
      onChange={(value) => update({ ...doc, required_checks: list(value) })} onBlur={() => undefined} error={fieldError(errors, "required_checks")} />
    <div className="recipe-grid">
      <TextField label="Max correction cycles" name="max-corrections" autoComplete="off" placeholder="3" value={String(doc.limits?.max_correction_cycles ?? "")} onChange={(value) => update({ ...doc, limits: { ...doc.limits, max_correction_cycles: int(value) } })} onBlur={() => undefined} error={fieldError(errors, "limits")} />
      <TextField label="Idle timeout (seconds)" name="timeout" autoComplete="off" placeholder="1800" value={String(doc.limits?.timeout_seconds ?? "")} onChange={(value) => update({ ...doc, limits: { ...doc.limits, timeout_seconds: int(value) } })} onBlur={() => undefined} />
      <TextField label="Max runtime (seconds, 0 = none)" name="max-runtime" autoComplete="off" placeholder="0" value={String(doc.limits?.max_runtime_seconds ?? 0)} onChange={(value) => update({ ...doc, limits: { ...doc.limits, max_runtime_seconds: int(value) } })} onBlur={() => undefined} required={false} />
    </div>
    <ProfilesEditor {...props} />
    <StagesEditor {...props} />
  </>;
}

function ProfilesEditor({ doc, update, errors, harnesses, allHarnesses, connections, skillPaths }: FormProps) {
  const entries = Object.entries(doc.profiles || {});
  const setProfile = (key: string, profile: RecipeProfile) => update({ ...doc, profiles: { ...doc.profiles, [key]: profile } });
  return <fieldset className="recipe-fieldset"><legend>Profiles</legend>
    {entries.map(([key, profile]) => <ProfileCard key={key} id={key} profile={profile} errors={errors} harnesses={harnesses.length ? harnesses : allHarnesses}
      allHarnesses={allHarnesses} connections={connections} skillPaths={skillPaths} usedBy={doc.stages.filter((s) => s.profile === key).length}
      onRename={(to) => { if (to && to !== key && !doc.profiles[to]) update(renameProfile(doc, key, to)); }}
      onChange={(next) => setProfile(key, next)}
      onRemove={() => { const { [key]: _removed, ...rest } = doc.profiles; update({ ...doc, profiles: rest }); }} />)}
    <button type="button" className="secondary-button" onClick={() => {
      let n = entries.length + 1;
      while (doc.profiles?.[`profile-${n}`]) n += 1;
      setProfile(`profile-${n}`, { harness: allHarnesses[0]?.harness || "claude-code", model: "", effort: allHarnesses[0]?.efforts[0] || "high" });
    }}><Plus size={15} aria-hidden="true" /> Add profile</button>
  </fieldset>;
}

function ProfileCard({ id, profile, errors, harnesses, allHarnesses, connections, skillPaths, usedBy, onRename, onChange, onRemove }: {
  id: string; profile: RecipeProfile; errors: RecipeValidationError[]; harnesses: FormProps["harnesses"]; allHarnesses: FormProps["harnesses"];
  connections: RecipeModelConnection[]; skillPaths?: string[]; usedBy: number; onRename: (to: string) => void; onChange: (p: RecipeProfile) => void; onRemove: () => void;
}) {
  const [draftId, setDraftId] = useState(id);
  const harness = allHarnesses.find((h) => h.harness === profile.harness);
  const eligible = connections.filter((c) => !harness?.provider || c.provider === harness.provider);
  const modelProvider = profile.harness === "opencode" ? profile.model.split("/")[0] : harness?.provider;
  // The connection only populates the model list; the recipe records harness/model/effort.
  const [chosenConnection, setConnectionId] = useState("");
  const connectionId = eligible.some((c) => c.id === chosenConnection) ? chosenConnection
    : eligible.find((c) => c.provider === modelProvider && c.models.some((m) => profile.model === m || profile.model === `${c.provider}/${m}`))?.id || eligible[0]?.id || "";
  const connection = eligible.find((c) => c.id === connectionId);
  // OpenCode records provider/model; the connection lists provider-native ids.
  const prefix = profile.harness === "opencode" && connection ? `${connection.provider}/` : "";
  const shownModel = prefix && profile.model.startsWith(prefix) ? profile.model.slice(prefix.length) : profile.model;
  // The effort list is the chosen model's (manifest or provider), else the harness's.
  const models = useConnectionModels(connectionId, harness ? profile.harness : "");
  const modelInfo = (id: string) => models.data?.models.find((m) => m.id === id);
  const efforts = effortChoices(modelInfo(shownModel), harness?.efforts || []);
  const setModel = (model: string, chosen = connectionId, info = modelInfo(model)) => {
    const provider = eligible.find((c) => c.id === chosen)?.provider;
    const pre = profile.harness === "opencode" && provider ? `${provider}/` : "";
    if (chosen !== connectionId) setConnectionId(chosen);
    onChange({ ...profile, model: pre && model && !model.includes("/") ? pre + model : model, effort: effortForModel(info, harness?.efforts || [], profile.effort) });
  };
  const skills = profile.skills || [];
  const at = `profiles.${id}`;
  return <div className="verification-check">
    <div className="verification-check-heading"><strong>Profile · {id}</strong>
      <button type="button" className="text-action" disabled={usedBy > 0} title={usedBy ? "Used by a stage" : undefined} onClick={onRemove}><Trash2 size={14} aria-hidden="true" /> Remove</button></div>
    <TextField label="Profile ID" name={`${at}-id`} autoComplete="off" placeholder="architect" value={draftId} onChange={setDraftId} onBlur={() => onRename(draftId.trim())} error={fieldError(errors, at)} />
    <div className="recipe-grid">
      <Select label="Harness" value={profile.harness} choices={harnesses.map((h) => [h.harness, h.harness])}
        onChange={(value) => onChange({ ...profile, harness: value, effort: allHarnesses.find((h) => h.harness === value)?.efforts.includes(profile.effort) ? profile.effort : allHarnesses.find((h) => h.harness === value)?.efforts[0] || profile.effort })} />
      <Select label="Effort" value={profile.effort} choices={efforts.map((e) => [e, e === modelInfo(shownModel)?.defaultEffort ? `${e} (model default)` : e])} onChange={(effort) => onChange({ ...profile, effort })} />
    </div>
    <ModelSelect connectionId={connectionId} connections={eligible.map((c) => ({ id: c.id, label: `${c.provider} · ${c.account}`, provider: c.provider }))}
      harness={harness ? profile.harness : ""} fieldId={`${at}-model`} value={shownModel} onChange={(model, chosen, info) => setModel(model, chosen || connectionId, info)} error={fieldError(errors, `${at}.model`)} />
    {!eligible.length ? <p className="form-hint">No active {harness?.provider || "model"} connection lists models yet; type the model ID under Advanced.</p> : null}
    {skillPaths ? <fieldset className="recipe-fieldset"><legend>Skills</legend>
      {[...new Set([...skillPaths, ...skills])].map((path) => <label key={path} className="recipe-check"><input type="checkbox" checked={skills.includes(path)}
        onChange={(event) => onChange({ ...profile, skills: event.target.checked ? [...skills, path] : skills.filter((s) => s !== path) })} /> <span className="mono">{path}</span></label>)}
      {!skillPaths.length && !skills.length ? <p className="form-hint">No SKILL.md files in the repository.</p> : null}
    </fieldset> : <TextField label="Skills (comma-separated repository paths)" name={`${at}-skills`} autoComplete="off" placeholder="skills/evidence/SKILL.md" value={skills.join(", ")}
      onChange={(value) => onChange({ ...profile, skills: list(value) })} onBlur={() => undefined} required={false} error={fieldError(errors, `${at}.skills`)} />}
  </div>;
}

function StagesEditor({ doc, update, errors, stageKinds, promptPaths, extensions, orgRecipe }: FormProps) {
  const setStage = (index: number, stage: RecipeStage) => update({ ...doc, stages: doc.stages.map((s, i) => i === index ? stage : s) });
  const profiles = Object.keys(doc.profiles || {});
  return <fieldset className="recipe-fieldset"><legend>Stages</legend>
    {doc.stages.map((stage, index) => <StageCard key={`${index}-${stage.id}`} index={index} stage={stage} doc={doc} errors={errors} stageKinds={stageKinds}
      profiles={profiles} promptPaths={promptPaths} extensions={extensions} orgRecipe={orgRecipe} onChange={(next) => setStage(index, next)}
      onRename={(to) => { if (to && to !== stage.id && !doc.stages.some((s) => s.id === to)) update(renameStage(doc, stage.id, to)); }}
      onRemove={() => update({ ...doc, stages: doc.stages.filter((_, i) => i !== index).map((s) => ({ ...s,
        ...(s.depends_on ? { depends_on: s.depends_on.filter((d) => d !== stage.id) } : {}) })) })} />)}
    <button type="button" className="secondary-button" onClick={() => {
      let n = doc.stages.length + 1;
      while (doc.stages.some((s) => s.id === `stage-${n}`)) n += 1;
      const last = doc.stages[doc.stages.length - 1];
      update({ ...doc, stages: [...doc.stages, { id: `stage-${n}`, kind: "review", profile: profiles[0] || "", prompt: "", depends_on: last ? [last.id] : [] }] });
    }}><Plus size={15} aria-hidden="true" /> Add stage</button>
  </fieldset>;
}

function StageCard({ index, stage, doc, errors, stageKinds, profiles, promptPaths, extensions, orgRecipe, onChange, onRename, onRemove }: {
  index: number; stage: RecipeStage; doc: RecipeDocument; errors: RecipeValidationError[]; stageKinds: string[]; profiles: string[]; promptPaths?: string[];
  extensions: ExtensionSet[]; orgRecipe: boolean;
  onChange: (s: RecipeStage) => void; onRename: (to: string) => void; onRemove: () => void;
}) {
  const [draftId, setDraftId] = useState(stage.id);
  const at = `stages[${index}]`;
  const human = stage.kind === "human_review";
  const others = doc.stages.filter((s) => s.id !== stage.id).map((s) => s.id);
  const deps = stage.depends_on || [];
  const loopable = stage.kind === "review" || stage.kind === "verify";
  const templates = extensions.flatMap((e) => templateChoices(e.versions, e.currentVersion, stage.kind, e.key));
  const template = templates.find((t) => t.reference === stage.template);
  const harness = stage.profile ? doc.profiles[stage.profile]?.harness : "";
  return <div className="verification-check">
    <div className="verification-check-heading"><strong>Stage {index + 1} · {stage.id}</strong><button type="button" className="text-action" onClick={onRemove}><Trash2 size={14} aria-hidden="true" /> Remove</button></div>
    <div className="recipe-grid">
      <TextField label="Stage ID" name={`${at}-id`} autoComplete="off" placeholder="implement" value={draftId} onChange={setDraftId} onBlur={() => onRename(draftId.trim())} error={fieldError(errors, `${at}.id`)} />
      <Select label="Kind" value={stage.kind} choices={stageKinds.map((k) => [k, k.replaceAll("_", " ")])} error={fieldError(errors, `${at}.kind`)}
        onChange={(kind) => onChange(kind === "human_review" ? { id: stage.id, kind, depends_on: stage.depends_on }
          : { ...stage, kind, profile: stage.profile || profiles[0] || "", prompt: stage.prompt || "", ...(kind === "review" || kind === "verify" ? {} : { loop: undefined }) })} />
      {!human ? <Select label="Profile" value={stage.profile || ""} choices={profiles.map((p) => [p, `${p} · ${doc.profiles[p]?.harness} ${doc.profiles[p]?.model}`])} onChange={(profile) => onChange({ ...stage, profile })} error={fieldError(errors, `${at}.profile`)} /> : null}
      {!human ? promptPaths?.length ? <Select label="Prompt file" value={stage.prompt || ""} choices={promptPaths.map((p) => [p, p])} onChange={(prompt) => onChange({ ...stage, prompt })} error={fieldError(errors, `${at}.prompt`)} />
        : <TextField label="Prompt file" name={`${at}-prompt`} autoComplete="off" placeholder="prompts/implement.md" value={stage.prompt || ""} onChange={(prompt) => onChange({ ...stage, prompt })} onBlur={() => undefined} error={fieldError(errors, `${at}.prompt`)} /> : null}
    </div>
    {!human ? <>
      <Select label="Extension template" value={stage.template || ""} error={fieldError(errors, `${at}.template`)}
        choices={[["", "None · the prompt file only"], ...templates.map((t): [string, string] => [t.reference, `${t.reference} · ${t.title}${t.current ? "" : " (older version)"}`])]}
        onChange={(value) => { const { template: _drop, ...rest } = stage; onChange(value ? { ...rest, template: value } : rest); }} />
      {template && harness && template.harness !== harness ? <p className="form-hint form-field-error">This template runs on {template.harness}; profile {stage.profile} uses {harness}.</p> : null}
      {!templates.length ? <p className="form-hint">{orgRecipe ? "No installed extension offers a template for this stage kind." : `No extension granted to this project offers a template for ${stage.kind.replaceAll("_", " ")} stages.`}</p>
        : orgRecipe ? <p className="form-hint">Lists every installed extension; a project launches it only with a grant.</p> : null}
    </> : null}
    <fieldset className="recipe-fieldset"><legend>Depends on</legend>
      {others.length ? others.map((id) => <label key={id} className="recipe-check"><input type="checkbox" checked={deps.includes(id)}
        onChange={(event) => onChange({ ...stage, depends_on: event.target.checked ? [...deps, id] : deps.filter((d) => d !== id) })} /> <span className="mono">{id}</span></label>)
        : <p className="form-hint">No other stages.</p>}
      {fieldError(errors, `${at}.depends_on`) ? <span className="form-field-error">{fieldError(errors, `${at}.depends_on`)}</span> : null}
    </fieldset>
    {loopable ? <>
      <label className="recipe-check"><input type="checkbox" checked={Boolean(stage.loop)} onChange={(event) => onChange(event.target.checked
        ? { ...stage, loop: { with: deps[0] || others[0] || "", until: "pass", max_cycles: 3 } } : { ...stage, loop: undefined })} /> Correction loop until pass</label>
      {stage.loop ? <div className="recipe-grid">
        <Select label="Correct with" value={stage.loop.with} choices={others.map((id) => [id, id])} onChange={(value) => onChange({ ...stage, loop: { ...stage.loop!, with: value } })} error={fieldError(errors, `${at}.loop`)} />
        <TextField label="Max cycles (1–10)" name={`${at}-cycles`} autoComplete="off" placeholder="3" value={String(stage.loop.max_cycles)} onChange={(value) => onChange({ ...stage, loop: { ...stage.loop!, max_cycles: int(value) } })} onBlur={() => undefined} />
      </div> : null}
    </> : null}
  </div>;
}
