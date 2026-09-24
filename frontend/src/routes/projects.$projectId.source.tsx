import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, GitBranch, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import type { ProjectSource } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getProject, getProjectSource, projectSourceQueryKey, setProjectSource } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/source")({ component: ProjectSourceSettings });

function ProjectSourceSettings() {
  const { projectId } = Route.useParams();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: Boolean(org && project.data?.project), queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const mayEdit = session.data?.role === "owner" || session.data?.role === "admin";

  return <PageShell>
    <PageHeader eyebrow="Project / Source" title="Git source" description={project.data?.project ? `Configure the repository for ${project.data.project.name}.` : "Configure the repository for this project."} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to project</Link>
    {project.isPending || (project.isSuccess && source.isPending) ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading project source</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {source.isError ? <div className="state-panel" role="alert"><h2>Source unavailable</h2><p>Source settings could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void source.refetch()}>Try again</button></div> : null}
    {project.data?.project && source.isSuccess && !mayEdit ? <div className="state-panel" role="note"><h2>Source settings are read-only</h2><p>Only organization owners and admins can change a project’s Git source.</p></div> : null}
    {project.data?.project && source.isSuccess && mayEdit ? <SourceEditor projectId={projectId} org={org} source={source.data} /> : null}
  </PageShell>;
}

function SourceEditor({ projectId, org, source }: { projectId: string; org: string; source: ProjectSource | null }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { repositoryUrl: source?.repositoryUrl || "", ref: source?.ref || "" },
    onSubmit: async ({ value }) => {
      setError("");
      const repositoryUrl = value.repositoryUrl.trim();
      const ref = value.ref.trim();
      if (repositoryUrl.length > 2048 || ref.length > 128) {
        setError("Use a repository URL of at most 2,048 characters and a ref of at most 128 characters.");
        return;
      }
      try {
        await setProjectSource(projectId, repositoryUrl, ref);
        await queryClient.invalidateQueries({ queryKey: projectSourceQueryKey(org, projectId) });
        await navigate({ to: "/projects/$projectId", params: { projectId } });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check the repository URL and ref. The source must be a public GitHub or GitLab HTTPS repository."
          : code === Code.PermissionDenied ? "Your session cannot change this source."
            : "Source could not be saved. Please try again.");
      }
    },
  });

  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="source-details-heading">
      <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="source-details-heading">Repository</h2><p>Choose the codebase and branch or tag for future runs.</p></div></div>
      <form className="editor-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="repositoryUrl">
          {(field) => <TextField autoFocus label="Repository URL" name={field.name} type="url" autoComplete="url" placeholder="https://github.com/organization/repository" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="ref" validators={{ onBlur: ({ value }) => value.trim().length <= 128 ? undefined : "Use at most 128 characters." }}>
          {(field) => <TextField label="Ref (optional)" name={field.name} autoComplete="off" placeholder="main" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} required={false} />}
        </form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects/$projectId" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button type="submit" className="primary-button" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : source ? <GitBranch size={15} aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Saving…" : source ? "Save changes" : "Add source"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    <aside className="editor-note"><h2>Current source support</h2><p>Public GitHub and GitLab repositories are supported over HTTPS. Leave the ref blank to use the remote default branch. Private repository credentials are not connected yet.</p></aside>
  </div>;
}
