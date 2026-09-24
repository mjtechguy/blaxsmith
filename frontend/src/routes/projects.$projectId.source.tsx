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
import {
  createGitConnection, getProject, getProjectSource, gitConnectionsQueryKey, launchAvailabilityQueryKey, listGitConnections,
  projectSourceQueryKey, setProjectSource,
} from "../workflow";

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
  const [adding, setAdding] = useState(false);
  const connections = useQuery({ queryKey: gitConnectionsQueryKey(org), queryFn: ({ signal }) => listGitConnections(signal) });
  const form = useForm({
    defaultValues: { repositoryUrl: source?.repositoryUrl || "", ref: source?.ref || "", gitConnectionId: source?.gitConnectionId || "" },
    onSubmit: async ({ value }) => {
      setError("");
      const repositoryUrl = value.repositoryUrl.trim();
      const ref = value.ref.trim();
      if (!repositoryUrl || repositoryUrl.length > 2048 || ref.length > 128) {
        setError("Enter a repository URL of at most 2,048 characters and a ref of at most 128 characters.");
        return;
      }
      try {
        await setProjectSource(projectId, repositoryUrl, ref, value.gitConnectionId);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: projectSourceQueryKey(org, projectId) }),
          queryClient.invalidateQueries({ queryKey: launchAvailabilityQueryKey(org, projectId) }),
        ]);
        await navigate({ to: "/projects/$projectId", params: { projectId } });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check the repository URL and ref. The source must be a GitHub or GitLab HTTPS repository."
          : code === Code.FailedPrecondition ? "The Git connection must be an active connection for this repository's host."
          : code === Code.PermissionDenied ? "Your session cannot change this source."
            : "Source could not be saved. Please try again.");
      }
    },
  });

  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="source-details-heading">
      <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="source-details-heading">Repository</h2><p>Choose the codebase and branch or tag for future runs.</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="repositoryUrl">
          {(field) => <TextField autoFocus label="Repository URL" name={field.name} type="url" autoComplete="url" placeholder="https://github.com/organization/repository" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="ref" validators={{ onBlur: ({ value }) => value.trim().length <= 128 ? undefined : "Use at most 128 characters." }}>
          {(field) => <TextField label="Ref (optional)" name={field.name} autoComplete="off" placeholder="main" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} required={false} />}
        </form.Field>
        <form.Field name="gitConnectionId">{(field) => <div className="form-field"><label htmlFor="source-git-connection">Git connection</label>
          <select id="source-git-connection" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}>
            <option value="">None (public repository)</option>
            {(connections.data ?? []).map((c) => <option key={c.id} value={c.id}>{c.username} @ {c.host}</option>)}
          </select>
          <small>Private repositories need a connection whose token can read and write this repository. Implement stages push a run branch through it.</small>
          <button type="button" className="text-action" onClick={() => setAdding(true)}><Plus size={15} aria-hidden="true" /> New Git connection</button>
        </div>}</form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects/$projectId" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button type="submit" className="primary-button" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : source ? <GitBranch size={15} aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Saving…" : source ? "Save changes" : "Add source"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    {adding ? <GitConnectionEditor org={org} onDone={(id) => { setAdding(false); if (id) form.setFieldValue("gitConnectionId", id); }} /> : null}
    <aside className="editor-note"><h2>Current source support</h2><p>GitHub and GitLab repositories over HTTPS. Public repositories need no connection. For a private repository, pick a Git connection; its token is used only by the platform to fetch the source and push the run branch, never by an agent. Leave the ref blank to use the remote default branch.</p></aside>
  </div>;
}

function GitConnectionEditor({ org, onDone }: { org: string; onDone: (id: string) => void }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { host: "github.com", username: "x-access-token", token: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const username = value.username.trim();
      const token = value.token.trim();
      if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(username) || !token || new TextEncoder().encode(token).length > 8192 || /[\s\0]/.test(token)) {
        setError("Enter a username of up to 128 letters, numbers, periods, underscores, or hyphens, and a single-line token of at most 8,192 bytes.");
        return;
      }
      form.setFieldValue("token", "");
      try {
        const created = await createGitConnection(value.host, username, token);
        await queryClient.invalidateQueries({ queryKey: gitConnectionsQueryKey(org) });
        onDone(created?.id ?? "");
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.PermissionDenied ? "Your session cannot add Git connections."
          : code === Code.InvalidArgument ? "Check the host, username, and token." : "The Git connection could not be added. Please try again.");
      }
    },
  });
  return <section className="editor-card" aria-labelledby="git-connection-heading">
    <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="git-connection-heading">New Git connection</h2><p>An organization token with contents read and write on the repository. It is write-only and never shown again.</p></div></div>
    <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <form.Field name="host">{(field) => <div className="form-field"><label htmlFor="git-connection-host">Host</label><select id="git-connection-host" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}><option value="github.com">GitHub</option><option value="gitlab.com">GitLab</option></select></div>}</form.Field>
      <form.Field name="username">
        {(field) => <TextField label="Username" name={field.name} autoComplete="off" placeholder="x-access-token" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
      </form.Field>
      <form.Field name="token">
        {(field) => <TextField label="Token" name={field.name} type="password" autoComplete="new-password" placeholder="Paste a fine-grained token" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
      </form.Field>
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <div className="editor-actions"><button type="button" className="secondary-button" onClick={() => onDone("")}>Cancel</button><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
        {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Adding…" : "Add Git connection"}</button>}
      </form.Subscribe></div>
    </form>
  </section>;
}
