import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, Plus, RefreshCw, UserRound } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import { PageHeader, PageShell } from "../page";
import { createSubscriptionConnection, getProject, subscriptionQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/model-access/subscription")({ component: ConnectSubscription });
const modelId = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

function ConnectSubscription() {
  const { projectId } = Route.useParams();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { model: "", credential: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const model = value.model.trim();
      const credential = value.credential.trim();
      if (!modelId.test(model) || !credential.startsWith("{") || new TextEncoder().encode(credential).length > 12288) {
        setError("Enter the exact model ID and paste the full contents of ~/.codex/auth.json.");
        return;
      }
      form.setFieldValue("credential", "");
      try {
        await createSubscriptionConnection(projectId, "openai", model, credential);
        await queryClient.invalidateQueries({ queryKey: subscriptionQueryKey(org, projectId) });
        await navigate({ to: "/projects/$projectId/model-access", params: { projectId } });
      } catch (cause) {
        const failure = ConnectError.from(cause);
        setError(failure.code === Code.InvalidArgument ? "That is not a ChatGPT sign-in auth.json with a refresh token. API-key logins belong under Add model access."
          : failure.code === Code.FailedPrecondition ? failure.rawMessage
            : "The subscription could not be connected. Please try again.");
      }
    },
  });

  return <PageShell>
    <PageHeader eyebrow="Project / Model access" title="Connect subscription" description="Use your own Codex ChatGPT sign-in for your runs in this project." />
    <Link to="/projects/$projectId/model-access" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to model access</Link>
    {project.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading project</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {project.data?.project ? <div className="editor-layout">
      <section className="editor-card" aria-labelledby="subscription-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserRound size={18} aria-hidden="true" /></span><div><h2 id="subscription-heading">Codex sign-in</h2><p>On your own machine run <span className="mono">codex login --device-auth</span>, then paste <span className="mono">~/.codex/auth.json</span>.</p></div></div>
        <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
          <form.Field name="model" validators={{ onBlur: ({ value }) => modelId.test(value.trim()) ? undefined : "Use an exact model ID of at most 128 letters, numbers, periods, underscores, or hyphens." }}>
            {(field) => <TextField label="Model ID" name={field.name} autoComplete="off" placeholder="Exact Codex model ID" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          <form.Field name="credential">
            {(field) => <TextField label="auth.json contents" name={field.name} type="password" autoComplete="new-password" placeholder="Paste ~/.codex/auth.json" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
          <div className="editor-actions"><Link to="/projects/$projectId/model-access" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
            {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Connecting…" : "Connect subscription"}</button>}
          </form.Subscribe></div>
        </form>
      </section>
      <aside className="editor-note"><h2>Personal and write-only</h2><p>Only your own runs can use this login; it is never shared with teammates or returned to the browser. Blaxsmith keeps the refresh token and gives each run a short-lived access token only. Claude subscriptions are not supported: Anthropic does not allow third-party platforms to store Claude.ai credentials.</p></aside>
    </div> : null}
  </PageShell>;
}
