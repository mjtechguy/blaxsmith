import { useEffect, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, BookCopy, GitBranch, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import type { ProjectSource, ProjectVerification } from "../gen/blaxsmith/api/v1/workflow_pb";
import { AccessExplanation } from "../access-explain";
import { useRepositoryInspection } from "../repo-inspect";
import { PageHeader, PageShell } from "../page";
import { StageDag } from "../recipe-pages";
import { getRecipe, getRecipeVersion, listRecipes, parseRecipe, recipeKey, recipesKey, recipeVersionKey, validateRecipe } from "../recipes";
import { getLaunchAvailability, getProject, getProjectSource, getProjectVerification, launchAvailabilityQueryKey, launchRun, projectSourceQueryKey, projectVerificationQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/runs/new")({ component: NewRun });

function NewRun() {
  const { projectId } = Route.useParams();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: Boolean(org && project.data?.project), queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: Boolean(org && project.data?.project), queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const availability = useQuery({ queryKey: launchAvailabilityQueryKey(org, projectId), enabled: Boolean(org && project.data?.project), queryFn: ({ signal }) => getLaunchAvailability(projectId, signal) });
  const mayLaunch = session.data?.role === "owner" || session.data?.role === "admin" || session.data?.role === "member";
  const mayConfigure = session.data?.role === "owner" || session.data?.role === "admin";

  return <PageShell>
    <PageHeader eyebrow="Project / New run" title="New run" description={project.data?.project ? `Freeze a recipe from ${project.data.project.name} into a durable execution plan.` : "Freeze a committed recipe into a durable execution plan."} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to project</Link>
    {project.isPending || (project.isSuccess && (source.isPending || verification.isPending || availability.isPending)) ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Checking run prerequisites</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {source.isError || verification.isError || availability.isError ? <div className="state-panel" role="alert"><h2>Prerequisites unavailable</h2><p>Source, verification, or run availability could not be loaded.</p><button type="button" className="secondary-button" onClick={() => { void source.refetch(); void verification.refetch(); void availability.refetch(); }}>Try again</button></div> : null}
    {project.data?.project && source.isSuccess && verification.isSuccess && availability.isSuccess && !mayLaunch ? <div className="state-panel" role="note"><h2>Run creation is restricted</h2><p>Organization owners, admins, and members can create runs.</p></div> : null}
    {project.data?.project && availability.data && mayLaunch && !availability.data.enabled ? <div className="state-panel" role="note"><h2>Run creation is unavailable</h2><p>{availability.data.reason}</p></div> : null}
    {project.data?.project && source.isSuccess && verification.isSuccess && mayLaunch && (!source.data || !verification.data) ? <div className="state-panel" role="note"><h2>Setup is required</h2><p>Configure a Git source and at least one verification check before creating a run.</p>
      {mayConfigure && !source.data ? <Link className="text-action" to="/projects/$projectId/source" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add Git source</Link> : null}
      {mayConfigure && !verification.data ? <Link className="text-action" to="/projects/$projectId/verification" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add verification checks</Link> : null}
    </div> : null}
    {project.data?.project && source.data && verification.data && availability.data?.enabled && mayLaunch ? <RunEditor projectId={projectId} org={org} source={source.data} verification={verification.data} /> : null}
  </PageShell>;
}

function RunEditor({ projectId, org, source, verification }: { projectId: string; org: string; source: ProjectSource; verification: ProjectVerification }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [launchKey] = useState(() => `run-${new Date().toISOString().slice(0, 19).replaceAll(":", "-")}-${crypto.randomUUID().slice(0, 8)}`);
  const recipes = useQuery({ queryKey: recipesKey(org, projectId), enabled: Boolean(org), queryFn: ({ signal }) => listRecipes(projectId, signal) });
  const [recipeId, setRecipeId] = useState("");
  // Preselect the repository's suggested recipe (by library name) once.
  const suggestion = useRepositoryInspection(projectId);
  const [preselected, setPreselected] = useState(false);
  useEffect(() => {
    const name = suggestion.data?.recipe;
    if (preselected || recipeId || !name || !recipes.data) return;
    const match = recipes.data.recipes.find((r) => r.name === name);
    if (match) setRecipeId(match.id);
    setPreselected(true);
  }, [preselected, recipeId, suggestion.data?.recipe, recipes.data]);
  const [versionChoice, setVersionChoice] = useState("");
  const chosenRecipe = recipes.data?.recipes.find((r) => r.id === recipeId);
  const recipe = useQuery({ queryKey: recipeKey(org, recipeId), enabled: Boolean(org && recipeId), queryFn: ({ signal }) => getRecipe(recipeId, signal) });
  const versionId = versionChoice || chosenRecipe?.currentVersionId || "";
  const version = useQuery({ queryKey: recipeVersionKey(org, versionId), enabled: Boolean(org && versionId), queryFn: ({ signal }) => getRecipeVersion(versionId, signal) });
  const order = useQuery({ queryKey: ["recipe-order", org, versionId], enabled: Boolean(org && version.data?.version), queryFn: ({ signal }) => validateRecipe(version.data?.version?.recipeJson || "", "", signal) });
  const form = useForm({
    defaultValues: { launchKey, recipePath: "", specPath: "", transcriptPath: "", scope: "." },
    onSubmit: async ({ value }) => {
      setError("");
      const fields = { launchKey: value.launchKey.trim(), recipePath: value.recipePath.trim(), specPath: value.specPath.trim(), transcriptPath: value.transcriptPath.trim(), scope: value.scope.trim() };
      // Advanced: a committed recipe path overrides the library choice.
      const libraryVersion = fields.recipePath ? "" : versionId;
      if (!fields.launchKey || fields.launchKey.length > 128 || (!fields.recipePath && !libraryVersion) || !fields.specPath || !fields.transcriptPath || !fields.scope) {
        setError("Enter a run key of at most 128 characters, choose a recipe version (or a committed recipe path), and enter the spec, transcript, and scope.");
        return;
      }
      try {
        const response = await launchRun(projectId, fields.launchKey, fields.recipePath, fields.specPath, fields.transcriptPath, fields.scope, libraryVersion);
        if (!response.run?.id) throw new Error("Run response did not include an ID");
        await queryClient.invalidateQueries({ queryKey: ["runs", org, projectId] });
        await navigate({ to: "/projects/$projectId/runs/$runId", params: { projectId, runId: response.run.id } });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        if (code === Code.FailedPrecondition) void queryClient.invalidateQueries({ queryKey: launchAvailabilityQueryKey(org, projectId) });
        setError(code === Code.FailedPrecondition ? "Run creation is unavailable or prerequisites changed. Check the Git source, committed input paths, scope, and required checks."
          : code === Code.InvalidArgument ? "Check the run key, repository paths, and scope."
            : code === Code.PermissionDenied ? "Your session cannot create this run."
              : "Run could not be created. Please try again.");
      }
    },
  });

  return <><div className="editor-layout">
    <section className="editor-card" aria-labelledby="run-inputs-heading">
      <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="run-inputs-heading">Run inputs</h2><p>Choose a library recipe version. Spec, transcript, prompt, and skill paths must exist in the commit selected when this run starts.</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <div className="notice"><strong>Guild recipe.</strong> Plan, implement, review, verify, architect review, then human review.{" "}
          <button type="button" className="text-action" onClick={() => {
            const guild = recipes.data?.recipes.find((r) => r.name === "Guild engineering");
            if (guild) { setRecipeId(guild.id); setVersionChoice(""); form.setFieldValue("recipePath", ""); } else form.setFieldValue("recipePath", "examples/guild/recipe.json");
            form.setFieldValue("specPath", "examples/guild/spec.md");
            form.setFieldValue("transcriptPath", "examples/guild/transcript.md");
          }}>Use the Guild example</button></div>
        <form.Field name="launchKey" validators={{ onBlur: ({ value }) => value.trim().length >= 1 && value.trim().length <= 128 ? undefined : "Use 1–128 characters." }}>
          {(field) => <TextField autoFocus label="Run key" name={field.name} autoComplete="off" placeholder="customer-portal-iteration-1" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <div className="recipe-grid">
          <label className="form-field"><span>Recipe</span>
            <select value={recipeId} onChange={(event) => { setRecipeId(event.target.value); setVersionChoice(""); }} disabled={!recipes.data}>
              <option value="">{recipes.isPending ? "Loading recipes…" : recipes.isError ? "Recipes unavailable" : "Choose a recipe…"}</option>
              {recipes.data?.recipes.map((r) => <option key={r.id} value={r.id}>{r.name}{r.projectId ? " · project" : " · organization"}</option>)}
            </select></label>
          <label className="form-field"><span>Version</span>
            <select value={versionId} onChange={(event) => setVersionChoice(event.target.value)} disabled={!recipe.data}>
              {!recipe.data ? <option value="">—</option> : null}
              {recipe.data?.versions.map((v) => <option key={v.id} value={v.id}>v{v.version}{v.id === recipe.data?.recipe?.currentVersionId ? " · current" : ""} · {v.sha256.slice(0, 10)}</option>)}
            </select></label>
        </div>
        {recipeId ? <AccessExplanation projectId={projectId} kind="recipe" resourceId={recipeId} /> : null}
        {preselected && suggestion.data?.recipe ? <p className="form-hint">{recipes.data?.recipes.some((r) => r.name === suggestion.data?.recipe)
          ? `Preselected “${suggestion.data.recipe}” from the repository's .blaxsmith.json.` : `The repository suggests “${suggestion.data.recipe}”, which is not available to this project.`}</p> : null}
        {version.data?.version ? <p className="form-hint">Freezes v{version.data.version.version} (<code>{version.data.version.sha256.slice(0, 12)}</code>) as <code>{version.data.version.frozenPath}</code>. Its prompts and skills are read from the repository commit.</p> : null}
        <p className="form-hint"><Link className="text-action" to="/projects/$projectId/recipes" params={{ projectId }}><BookCopy size={14} aria-hidden="true" /> Manage recipes</Link></p>
        <details className="recipe-advanced"><summary>Advanced: use a committed recipe file</summary>
          <form.Field name="recipePath">{(field) => <TextField label="Recipe path (overrides the library choice)" name={field.name} autoComplete="off" placeholder="automation/recipe.json" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} required={false} />}</form.Field>
        </details>
        <form.Field name="specPath">{(field) => <TextField label="Spec path" name={field.name} autoComplete="off" placeholder="docs/spec.md" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}</form.Field>
        <form.Field name="transcriptPath">{(field) => <TextField label="Transcript path" name={field.name} autoComplete="off" placeholder="docs/transcript.md" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}</form.Field>
        <form.Field name="scope">{(field) => <TextField label="Code scope" name={field.name} autoComplete="off" placeholder="." value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}</form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects/$projectId" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Creating…" : "Create run"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    <aside className="editor-note"><h2>Run prerequisites</h2>
      <dl className="launch-prerequisites"><div><dt>Git source</dt><dd>{source.repositoryUrl}<br /><span>{source.ref || "Remote default branch"}</span></dd></div>
        <div><dt>Verification · Version {verification.version.toString()}</dt><dd><ol>{verification.checks.map((check) => <li key={check.id}><strong>{check.id}</strong><code>{JSON.stringify(check.command)}</code></li>)}</ol></dd></div></dl>
      <p>Pick a recipe version from the library. The server freezes that version's exact bytes with the spec, transcript, prompts, and skills from the resolved commit, plus the verification policy, at launch.</p>
      <p>The run key makes duplicate launch requests idempotent. Use a new key for a distinct run.</p>
    </aside>
  </div>
  {version.data?.version ? <StageDag doc={parseRecipe(version.data.version.recipeJson)} order={order.data?.stageOrder} title={`Stage graph · ${chosenRecipe?.name || "recipe"} v${version.data.version.version}`} /> : null}
  </>;
}
