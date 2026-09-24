import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, KeyRound, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import { PageHeader, PageShell } from "../page";
import { createProjectModelAccess, getProject, projectModelAccessQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/model-access/new")({ component: AddModelAccess });
const modelId = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

function AddModelAccess() {
  const { projectId } = Route.useParams();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const mayAdd = session.data?.role === "owner" || session.data?.role === "admin";

  return <PageShell>
    <PageHeader eyebrow="Project / Model access" title="Add model access" description={project.data?.project ? `Connect a provider key and grant a model to ${project.data.project.name}.` : "Connect a provider key and grant a model to this project."} />
    <Link to="/projects/$projectId/model-access" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to model access</Link>
    {project.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading project</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {project.data?.project && !mayAdd ? <div className="state-panel" role="note"><h2>Model access is read-only</h2><p>Only organization owners and admins can add provider credentials and project grants.</p></div> : null}
    {project.data?.project && mayAdd ? <ModelAccessEditor projectId={projectId} org={org} /> : null}
  </PageShell>;
}

function ModelAccessEditor({ projectId, org }: { projectId: string; org: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { provider: "openai", model: "", apiKey: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const provider = value.provider;
      const model = value.model.trim();
      const apiKey = value.apiKey.trim();
      if ((provider !== "openai" && provider !== "anthropic") || !modelId.test(model) ||
        !apiKey || new TextEncoder().encode(apiKey).length > 8192 || /[\r\n\0]/.test(apiKey)) {
        setError("Choose a provider, enter its exact model ID (up to 128 letters, numbers, periods, underscores, or hyphens), and paste a single-line API key of at most 8,192 bytes.");
        return;
      }
      form.setFieldValue("apiKey", "");
      try {
        await createProjectModelAccess(projectId, provider, model, apiKey);
        await queryClient.invalidateQueries({ queryKey: projectModelAccessQueryKey(org, projectId) });
        await navigate({ to: "/projects/$projectId/model-access", params: { projectId } });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check the provider, exact model ID, and API key."
          : code === Code.AlreadyExists || code === Code.FailedPrecondition ? "This model may already have access, or credential custody is unavailable. Check project access and try again."
            : code === Code.PermissionDenied ? "Your session cannot add model access."
              : "Model access could not be added. Please try again.");
      }
    },
  });

  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="model-access-details-heading">
      <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="model-access-details-heading">Provider credential</h2><p>Create an organization-owned connection and grant this model to the project.</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="provider">{(field) => <div className="form-field"><label htmlFor="model-access-provider">Provider</label><select id="model-access-provider" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option></select></div>}</form.Field>
        <form.Field name="model" validators={{ onBlur: ({ value }) => modelId.test(value.trim()) ? undefined : "Use an exact model ID of at most 128 letters, numbers, periods, underscores, or hyphens." }}>
          {(field) => <TextField label="Model ID" name={field.name} autoComplete="off" placeholder="Exact provider model ID" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="apiKey">
          {(field) => <TextField label="API key" name={field.name} type="password" autoComplete="new-password" placeholder="Paste provider API key" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects/$projectId/model-access" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Adding…" : "Add model access"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    <aside className="editor-note"><h2>Write-only key</h2><p>The API key is sent once to create an organization connection. It is never returned or shown in the project workspace after creation.</p></aside>
  </div>;
}
