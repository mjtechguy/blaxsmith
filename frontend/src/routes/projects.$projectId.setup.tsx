import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ArrowRight, BookCopy, KeyRound, Play } from "lucide-react";
import { CreateFlow, SummaryList } from "../layouts";
import { flowOrder, projectFlowSteps, SourceEditor, VerificationEditor, type FlowStepId } from "../project-settings";
import { listRecipes, recipesKey } from "../recipes";
import { suggestionSource, useRepositoryInspection } from "../repo-inspect";
import { Card, EmptyState, StatePanel } from "../ui";
import { useScope } from "../workspace-ui";
import {
  getProject, getProjectSource, getProjectVerification, listProjectModelAccess, projectModelAccessQueryKey, projectSourceQueryKey, projectVerificationQueryKey,
} from "../workflow";

type Step = Exclude<FlowStepId, "details">;
export const Route = createFileRoute("/projects/$projectId/setup")({
  validateSearch: (search: Record<string, unknown>): { step: Step } => ({ step: (["source", "verification", "recipe", "access"] as const).find((s) => s === search.step) ?? "source" }),
  component: ProjectSetup,
});

// Steps after a project exists: source → verification → recipe → access. Each can be skipped and finished later in Settings.
function ProjectSetup() {
  const { projectId } = Route.useParams();
  const { step } = Route.useSearch();
  const navigate = useNavigate();
  const { org, isAdmin, session } = useScope();
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const ready = Boolean(org && project.data?.project);
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const recipes = useQuery({ queryKey: recipesKey(org, projectId), enabled: ready, queryFn: ({ signal }) => listRecipes(projectId, signal) });
  const access = useQuery({ queryKey: projectModelAccessQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => listProjectModelAccess(projectId, signal) });
  // The repository's suggested recipe (.blaxsmith.json) is preselected; the user confirms by starting a run with it.
  const inspection = useRepositoryInspection(projectId, Boolean(source.data));
  const suggestedRecipe = recipes.data?.recipes.find((r) => r.name === inspection.data?.recipe);
  const [picked, setPicked] = useState("");
  const recipeId = picked || suggestedRecipe?.id || "";
  const next = () => {
    const following = flowOrder[flowOrder.indexOf(step) + 1] as Step | undefined;
    void navigate(following ? { to: "/projects/$projectId/setup", params: { projectId }, search: { step: following } } : { to: "/projects/$projectId", params: { projectId } });
  };
  const skip = <button type="button" className="secondary-button" onClick={next}>Skip for now</button>;
  const done = { details: true, source: Boolean(source.data), verification: Boolean(verification.data), recipe: Boolean(recipes.data?.recipes.length), access: Boolean(access.data?.access.length) };

  if (project.isPending || session.isPending) return <StatePanel kind="loading" title="Loading project" />;
  if (!project.data?.project) return <StatePanel kind="error" title="Project unavailable" retry={() => void project.refetch()} />;
  const p = project.data.project;
  return <CreateFlow title={`Set up ${p.name}`} description="Finish what a run needs. Every step can be changed later from the project’s Settings."
    steps={projectFlowSteps(step, projectId, done)}
    summary={<>
      <h2>Summary</h2>
      <SummaryList items={[
        { label: "Project", value: p.name, done: true },
        { label: "Source", value: source.data ? source.data.repositoryUrl.replace(/^https:\/\//, "") : "Not set", done: Boolean(source.data) },
        { label: "Verification", value: verification.data ? `${verification.data.checks.length} checks` : "Not set", done: Boolean(verification.data) },
        { label: "Recipes", value: recipes.data ? `${recipes.data.recipes.length} available` : "…", done: done.recipe },
        { label: "Model access", value: access.data ? `${access.data.access.length} grants` : "…", done: done.access },
      ]} />
      <Link className="text-action" to="/projects/$projectId" params={{ projectId }}>Go to the project overview <ArrowRight size={13} aria-hidden="true" /></Link>
    </>}>
    {(step === "source" || step === "verification") && !isAdmin ? <Card title={step === "source" ? "Git source" : "Verification checks"}>
      <p className="card-body">Only organization owners and admins configure {step === "source" ? "the Git source" : "verification checks"}. Ask one to finish this step; you can continue with the others.</p>
      <div className="editor-actions card-body"><button type="button" className="primary-button" onClick={next}>Continue</button></div></Card> : null}
    {step === "source" && isAdmin ? source.isPending ? <StatePanel kind="loading" title="Loading source" />
      : <SourceEditor projectId={projectId} org={org} source={source.data ?? null} mode={{ kind: "flow", next, skip }} /> : null}
    {step === "verification" && isAdmin ? verification.isPending ? <StatePanel kind="loading" title="Loading checks" />
      : <VerificationEditor projectId={projectId} org={org} current={verification.data ?? null} mode={{ kind: "flow", next, skip }} /> : null}
    {step === "recipe" ? <Card title={<><BookCopy size={15} aria-hidden="true" /> Recipe</>} description="A recipe is the stage graph a run follows. Runs pick a version when they launch.">
      <div className="card-body">
        {inspection.data?.recipe ? <p className="form-hint">{suggestedRecipe ? `Preselected “${suggestedRecipe.name}”, suggested by ${suggestionSource(inspection.data)}.`
          : `The repository suggests “${inspection.data.recipe}”, which is not available to this project yet.`}</p> : null}
        {recipes.data?.recipes.length ? <fieldset className="recipe-fieldset"><legend>Recipe for the first run</legend>
          {recipes.data.recipes.slice(0, 8).map((r) => <label key={r.id} className="recipe-check"><input type="radio" name="setup-recipe" value={r.id} checked={recipeId === r.id} onChange={() => setPicked(r.id)} />
            <span>{r.name} <small className="muted">{r.projectId ? "project" : "organization"} · v{r.currentVersion}{r.id === suggestedRecipe?.id ? " · suggested" : ""}</small></span></label>)}
        </fieldset>
          : <EmptyState title="No recipes are available yet">Create a project recipe, or ask an admin to grant an organization recipe to this project.</EmptyState>}
      </div>
      <div className="editor-actions card-body">
        <Link className="secondary-button" to="/projects/$projectId/recipes" params={{ projectId }}>Open recipes</Link>
        {recipeId ? <Link className="secondary-button" to="/projects/$projectId/runs/new" params={{ projectId }} search={{ recipe: recipeId }}><Play size={15} aria-hidden="true" /> Start a run with it</Link> : null}
        <button type="button" className="primary-button" onClick={next}>Continue</button>
      </div>
    </Card> : null}
    {step === "access" ? <Card title={<><KeyRound size={15} aria-hidden="true" /> Access</>} description="Model access lets runs call a provider. Keys are write-only; agents receive short-lived leases.">
      <div className="card-body">{access.data?.access.length ? <p>{access.data.access.length} model {access.data.access.length === 1 ? "grant" : "grants"} attached.</p>
        : <p>No model access yet. Add a project key, use an organization connection granted to this project, or use one of your own.</p>}</div>
      <div className="editor-actions card-body">
        <Link className="secondary-button" to="/projects/$projectId/connections" params={{ projectId }}>Open connections</Link>
        <button type="button" className="primary-button" onClick={next}>Finish setup</button>
      </div>
    </Card> : null}
  </CreateFlow>;
}
