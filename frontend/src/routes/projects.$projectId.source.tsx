import { useEffect, useMemo, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, GitBranch, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { useConnections } from "../connection-ui";
import { listGitBranches, listGitRepositories, providerLabel } from "../connections";
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
  const owned = useConnections("project", projectId);
  const granted = useConnections("project_available", projectId);
  // Organization Git connections plus project-owned and granted ones, once each.
  const gitChoices = useMemo(() => {
    const seen = new Map<string, string>();
    for (const c of connections.data ?? []) seen.set(c.id, `${c.username} @ ${c.host}`);
    for (const c of [...(owned.data ?? []), ...(granted.data ?? [])]) if (c.kind === "git" && !seen.has(c.id)) seen.set(c.id, `${c.account} @ ${providerLabel(c.provider)} (${c.scope})`);
    return [...seen].map(([id, label]) => ({ id, label }));
  }, [connections.data, owned.data, granted.data]);
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
        <form.Field name="gitConnectionId">{(field) => <div className="form-field"><label htmlFor="source-git-connection">Git connection</label>
          <select id="source-git-connection" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}>
            <option value="">None (public repository)</option>
            {gitChoices.map((c) => <option key={c.id} value={c.id}>{c.label}</option>)}
          </select>
          <small>Private repositories need a connection whose token can read and write this repository. Implement stages push a run branch through it.</small>
          <button type="button" className="text-action" onClick={() => setAdding(true)}><Plus size={15} aria-hidden="true" /> New Git connection</button>
        </div>}</form.Field>
        <form.Subscribe selector={(state) => state.values.gitConnectionId}>
          {(connectionId) => {
            const urlField = <form.Field name="repositoryUrl">
              {(field) => <TextField autoFocus={!connectionId} label="Repository URL" name={field.name} type="url" autoComplete="url" placeholder="https://github.com/organization/repository" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
            </form.Field>;
            const refField = <form.Field name="ref" validators={{ onBlur: ({ value }) => value.trim().length <= 128 ? undefined : "Use at most 128 characters." }}>
              {(field) => <TextField label="Ref (optional)" name={field.name} autoComplete="off" placeholder="main" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} required={false} />}
            </form.Field>;
            if (!connectionId) return <>{urlField}{refField}</>;
            return <>
              <form.Subscribe selector={(state) => [state.values.repositoryUrl, state.values.ref] as const}>
                {([repositoryUrl, ref]) => <RepoPicker connectionId={connectionId} repositoryUrl={repositoryUrl} gitRef={ref}
                  onRepository={(url, branch) => { form.setFieldValue("repositoryUrl", url); form.setFieldValue("ref", branch); }}
                  onRef={(branch) => form.setFieldValue("ref", branch)} />}
              </form.Subscribe>
              <details className="advanced-disclosure"><summary>Advanced: type a repository URL and ref</summary>{urlField}{refField}</details>
            </>;
          }}
        </form.Subscribe>
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

// Repositories and branches the chosen connection can see, as searchable dropdowns.
function RepoPicker({ connectionId, repositoryUrl, gitRef, onRepository, onRef }: {
  connectionId: string; repositoryUrl: string; gitRef: string; onRepository: (url: string, branch: string) => void; onRef: (branch: string) => void;
}) {
  const [search, setSearch] = useState("");
  const [query, setQuery] = useState("");
  useEffect(() => { const t = window.setTimeout(() => setQuery(search.trim()), 300); return () => window.clearTimeout(t); }, [search]);
  const repos = useQuery({ queryKey: ["git-repositories", connectionId, query], queryFn: ({ signal }) => listGitRepositories(connectionId, query, signal) });
  const selected = repos.data?.find((r) => r.cloneUrl === repositoryUrl);
  const branches = useQuery({ queryKey: ["git-branches", connectionId, selected?.fullName ?? ""], enabled: Boolean(selected),
    queryFn: ({ signal }) => listGitBranches(connectionId, selected!.fullName, signal) });
  return <>
    <TextField label="Search repositories" name="repository-search" autoComplete="off" placeholder="owner/name" value={search} onChange={setSearch} onBlur={() => {}} required={false} />
    <div className="form-field"><label htmlFor="source-repository">Repository</label>
      <select id="source-repository" value={selected?.cloneUrl ?? ""} disabled={repos.isPending}
        onChange={(event) => { const repo = repos.data?.find((r) => r.cloneUrl === event.target.value); if (repo) onRepository(repo.cloneUrl, repo.defaultBranch); }}>
        <option value="">{repos.isPending ? "Loading repositories…" : repos.isError ? "Repositories could not be loaded" : "Choose a repository"}</option>
        {(repos.data ?? []).map((r) => <option key={r.fullName} value={r.cloneUrl}>{r.fullName}{r.private ? " (private)" : ""}</option>)}
      </select>
      {repositoryUrl && !selected && repos.isSuccess ? <small>Current: <span className="mono">{repositoryUrl}</span></small> : null}
    </div>
    {selected ? <div className="form-field"><label htmlFor="source-branch">Branch</label>
      <select id="source-branch" value={gitRef} disabled={branches.isPending} onChange={(event) => onRef(event.target.value)}>
        <option value="">Remote default branch</option>
        {(branches.data ?? []).map((b) => <option key={b} value={b}>{b}</option>)}
      </select></div> : null}
  </>;
}
