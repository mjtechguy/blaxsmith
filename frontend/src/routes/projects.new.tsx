import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, FolderPlus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import { PageHeader, PageShell } from "../page";
import { createProject } from "../workflow";

export const Route = createFileRoute("/projects/new")({ component: NewProject });
const slugPattern = /^[a-z][a-z0-9-]{2,63}$/;

function NewProject() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { name: "", slug: "" },
    onSubmit: async ({ value }) => {
      setError("");
      if (value.name.trim().length < 1 || value.name.trim().length > 160 || !slugPattern.test(value.slug.trim())) {
        setError("Check the project name and URL name before creating it.");
        return;
      }
      try {
        const response = await createProject(value.slug.trim(), value.name.trim());
        await queryClient.invalidateQueries({ queryKey: ["projects", session.data?.organizationId || ""] });
        if (response.project?.id) await navigate({ to: "/projects/$projectId", params: { projectId: response.project.id } });
        else setError("Workspace was created, but its page could not be opened. Return to the workspace list and open it there.");
      } catch (cause) {
        setError(ConnectError.from(cause).code === Code.AlreadyExists ? "That workspace URL is already in use." : "Workspace could not be created. Please try again.");
      }
    },
  });

  return <PageShell>
    <PageHeader eyebrow="Workspaces / New" title="Create a workspace" description="Set up a shared home for a repository, model access, verification, and agent runs." />
    <Link to="/" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to workspaces</Link>
    <div className="editor-layout">
      <section className="editor-card" aria-labelledby="project-details-heading">
        <div className="editor-card-heading"><span className="project-symbol"><FolderPlus size={18} aria-hidden="true" /></span><div><h2 id="project-details-heading">Workspace details</h2><p>Choose how this workspace appears to your organization.</p></div></div>
        <form className="editor-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
          <form.Field name="name" validators={{ onBlur: ({ value }) => value.trim().length >= 1 && value.trim().length <= 160 ? undefined : "Use 1–160 characters." }}>
            {(field) => <TextField autoFocus label="Workspace name" name={field.name} autoComplete="off" placeholder="Customer portal" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          <form.Field name="slug" validators={{ onBlur: ({ value }) => slugPattern.test(value.trim()) ? undefined : "Use 3–64 lowercase letters, numbers, or hyphens; start with a letter." }}>
            {(field) => <TextField label="Workspace URL name" name={field.name} autoComplete="off" placeholder="customer-portal" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
          <div className="editor-actions"><Link to="/" className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
            {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <FolderPlus size={15} aria-hidden="true" />}{submitting ? "Creating…" : "Create workspace"}</button>}
          </form.Subscribe></div>
        </form>
      </section>
      <aside className="editor-note"><h2>Next, connect the work</h2><p>After creating it, add a public GitHub or GitLab repository, configure verification, and grant model access if your recipe needs it. Runs use files already committed to Git.</p><p>This workspace organizes runs; it does not create a persistent AX pod or provide an interactive terminal session.</p></aside>
    </div>
  </PageShell>;
}
