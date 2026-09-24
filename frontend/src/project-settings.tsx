// Project source and verification editors, used as Settings sections (with a
// sticky save bar and unsaved-change guard) and as steps of the new-project flow.
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { FileSearch, GitBranch, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react";
import { useConnections } from "./connection-ui";
import { listGitBranches, listGitRepositories, providerLabel } from "./connections";
import { TextField } from "./form-field";
import type { ProjectSource, ProjectVerification } from "./gen/blaxsmith/api/v1/workflow_pb";
import { GuardedSaveBar, useSaved } from "./layouts";
import { RepositorySuggestion, useRepositoryInspection } from "./repo-inspect";
import { inspectKey, inspectRepository } from "./setup";
import { Disclosure } from "./ui";
import {
  createGitConnection, gitConnectionsQueryKey, launchAvailabilityQueryKey, listGitConnections, projectSourceQueryKey,
  projectVerificationQueryKey, setProjectSource, setProjectVerification,
} from "./workflow";

export const flowOrder = ["details", "source", "verification", "recipe", "access"] as const;
export type FlowStepId = (typeof flowOrder)[number];
const flowLabels: Record<FlowStepId, string> = { details: "Details", source: "Source", verification: "Verification", recipe: "Recipe", access: "Access" };

// Steps of the new-project flow; after creation each step links to its setup page.
export function projectFlowSteps(current: FlowStepId, projectId?: string, done: Partial<Record<FlowStepId, boolean>> = {}) {
  const at = flowOrder.indexOf(current);
  return flowOrder.map((id, index) => ({
    id, label: flowLabels[id],
    state: id === current ? "current" as const : index < at || done[id] ? "done" as const : "todo" as const,
    href: projectId && id !== "details" ? `/projects/${projectId}/setup` : undefined, search: { step: id },
  }));
}

export type EditorMode ={ kind: "settings" } | { kind: "flow"; next: () => void; skip: ReactNode };

function FlowActions({ mode, submitting, canSubmit, label }: { mode: Extract<EditorMode, { kind: "flow" }>; submitting: boolean; canSubmit: boolean; label: string }) {
  return <div className="editor-actions">{mode.skip}
    <button type="submit" className="primary-button" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : null}{submitting ? "Saving…" : label}</button></div>;
}

export function SourceEditor({ projectId, org, source, mode }: { projectId: string; org: string; source: ProjectSource | null; mode: EditorMode }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [saved, setSaved] = useSaved();
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
        // A new source means new suggestions: read the repository now, so the
        // verification and recipe steps can prefill from it.
        queryClient.removeQueries({ queryKey: inspectKey(org, projectId) });
        void queryClient.prefetchQuery({ queryKey: inspectKey(org, projectId), queryFn: ({ signal }) => inspectRepository(projectId, signal), staleTime: 5 * 60_000, retry: false });
        form.reset({ repositoryUrl, ref, gitConnectionId: value.gitConnectionId });
        setSaved(true);
        if (mode.kind === "flow") mode.next();
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check the repository URL and ref. The source must be a GitHub or GitLab HTTPS repository."
          : code === Code.FailedPrecondition ? "The Git connection must be an active connection for this repository's host."
            : code === Code.PermissionDenied ? "Your session cannot change this source."
              : "Source could not be saved. Please try again.");
      }
    },
  });

  return <div className="settings-section">
    <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <section className="editor-card" aria-labelledby="source-details-heading">
        <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="source-details-heading">Repository</h2><p>The codebase and branch or tag future runs start from. Leave the ref blank to use the remote default branch.</p></div></div>
        <div className="editor-form">
          <form.Field name="gitConnectionId">{(field) => <div className="form-field"><label htmlFor="source-git-connection">Git connection</label>
            <select id="source-git-connection" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}>
              <option value="">None (public repository)</option>
              {gitChoices.map((c) => <option key={c.id} value={c.id}>{c.label}</option>)}
            </select>
            <small className="form-hint">Private repositories need a connection whose token can read and write the repository. The platform uses it; agents never receive it.</small>
            <button type="button" className="text-action" aria-expanded={adding} onClick={() => setAdding(!adding)}><Plus size={15} aria-hidden="true" /> New Git connection</button>
          </div>}</form.Field>
          <form.Subscribe selector={(state) => state.values.gitConnectionId}>
            {(connectionId) => {
              const urlField = <form.Field name="repositoryUrl">
                {(field) => <TextField label="Repository URL" name={field.name} type="url" autoComplete="url" placeholder="https://github.com/organization/repository" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
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
                <Disclosure summary="Advanced: type a repository URL and ref">{urlField}{refField}</Disclosure>
              </>;
            }}
          </form.Subscribe>
          {error && mode.kind === "flow" ? <p className="auth-alert" role="alert">{error}</p> : null}
        </div>
      </section>
      {source ? <section className="editor-card" aria-labelledby="repo-suggestion-heading">
        <div className="editor-card-heading"><span className="project-symbol"><FileSearch size={18} aria-hidden="true" /></span><div><h2 id="repo-suggestion-heading">What this repository suggests</h2>
          <p>Read from .blaxsmith.json at the pinned commit, or detected from its manifests. Nothing is saved until you confirm.</p></div></div>
        <div className="editor-form"><RepositorySuggestion projectId={projectId} reviewLink={mode.kind === "settings"} /></div>
      </section> : null}
      <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
        {([dirty, submitting, canSubmit]) => mode.kind === "flow" ? <FlowActions mode={mode} submitting={submitting} canSubmit={canSubmit} label="Save source and continue" />
          : <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved} onCancel={() => { form.reset(); setError(""); }} saveLabel={source ? "Save changes" : "Add source"} />}
      </form.Subscribe>
    </form>
    {adding ? <GitConnectionEditor org={org} onDone={(id) => { setAdding(false); if (id) form.setFieldValue("gitConnectionId", id); }} /> : null}
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

// Repositories and branches the chosen connection can see.
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

const checkId = /^[a-z][a-z0-9_-]{0,63}$/;

export function VerificationEditor({ projectId, org, current, mode }: { projectId: string; org: string; current: ProjectVerification | null; mode: EditorMode }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [saved, setSaved] = useSaved();
  const initial = () => current?.checks.map((check) => ({ id: check.id, command: [...check.command] })) || [{ id: "", command: [""] }];
  // Suggestions from .blaxsmith.json or detection prefill an empty policy; an
  // existing one is replaced only on request. Nothing is saved until the user confirms.
  const inspection = useRepositoryInspection(projectId);
  const suggested = inspection.data?.verification.map((c) => ({ id: c.id, command: [...c.command] })) ?? [];
  const [prefilled, setPrefilled] = useState(false);
  const form = useForm({
    defaultValues: { checks: initial() },
    onSubmit: async ({ value }) => {
      setError("");
      const checks = value.checks.map((check) => ({ id: check.id.trim(), command: check.command }));
      if (checks.length < 1 || checks.length > 64 || new Set(checks.map((check) => check.id)).size !== checks.length ||
        checks.some((check) => !checkId.test(check.id) || check.command.length < 1 || check.command.length > 32 ||
          check.command.some((part) => !part || part.length > 4096 || part.includes("\0")))) {
        setError("Use 1–64 checks with unique lowercase IDs and 1–32 nonempty command arguments each.");
        return;
      }
      try {
        await setProjectVerification(projectId, checks);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: projectVerificationQueryKey(org, projectId) }),
          queryClient.invalidateQueries({ queryKey: launchAvailabilityQueryKey(org, projectId) }),
        ]);
        form.reset({ checks });
        setSaved(true);
        if (mode.kind === "flow") mode.next();
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check IDs and command arguments are invalid. Use unique lowercase IDs and separate argv fields."
          : code === Code.PermissionDenied ? "Your session cannot change verification."
            : "Verification could not be saved. Please try again.");
      }
    },
  });

  useEffect(() => {
    if (current || prefilled || !suggested.length || form.state.isDirty) return;
    form.setFieldValue("checks", suggested);
    setPrefilled(true);
  }, [current, prefilled, suggested.length]);
  const applySuggested = () => { form.setFieldValue("checks", suggested); setPrefilled(true); };

  return <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="checks-heading">
      <div className="editor-card-heading"><span className="project-symbol"><ShieldCheck size={18} aria-hidden="true" /></span><div><h2 id="checks-heading">Checks</h2>
        <p>{current ? `Version ${current.version.toString()}${current.updatedAt ? ` · updated ${new Date(current.updatedAt).toLocaleString()}` : ""}. ` : "At least one check is required to launch a run. "}Enter the executable and each argument separately; shell syntax is not parsed.</p></div></div>
      <div className="editor-form">
        <RepositorySuggestion projectId={projectId} />
        {prefilled ? <p className="notice" role="status">These checks were prefilled from the repository. Review them, then {mode.kind === "flow" ? "save and continue" : "save"} to confirm.</p> : null}
        {current && suggested.length && !prefilled ? <button type="button" className="secondary-button" onClick={applySuggested}>Use suggested checks</button> : null}
        <form.Field name="checks" mode="array">{(checksField) => <>
          {checksField.state.value.map((check, checkIndex) => <div className="verification-check" key={checkIndex}>
            <div className="verification-check-heading"><strong>Check {checkIndex + 1}{check.id ? ` · ${check.id}` : ""}</strong><button type="button" className="text-action" disabled={checksField.state.value.length === 1} onClick={() => checksField.removeValue(checkIndex)}><Trash2 size={14} aria-hidden="true" /> Remove</button></div>
            <form.Field name={`checks[${checkIndex}].id`} validators={{ onBlur: ({ value }) => checkId.test(value.trim()) ? undefined : "Use a lowercase ID starting with a letter, up to 64 characters." }}>
              {(field) => <TextField label="Check ID" name={field.name} autoComplete="off" placeholder="unit-tests" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
            </form.Field>
            <p className="form-hint mono">{check.command.filter(Boolean).join(" ") || "No command yet"}</p>
            <Disclosure summary={`Command arguments (${check.command.length})`} defaultOpen={!current}>
              <form.Field name={`checks[${checkIndex}].command`} mode="array">{(commandField) => <div className="verification-arguments">
                {commandField.state.value.map((_, argIndex) => <div className="verification-argument" key={argIndex}>
                  <form.Field name={`checks[${checkIndex}].command[${argIndex}]`}>
                    {(field) => <TextField label={argIndex === 0 ? "Executable" : `Argument ${argIndex}`} name={field.name} autoComplete="off" placeholder={argIndex === 0 ? "npm" : "test"} value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
                  </form.Field>
                  {argIndex > 0 ? <button type="button" className="secondary-button" aria-label={`Remove argument ${argIndex} from check ${checkIndex + 1}`} onClick={() => commandField.removeValue(argIndex)}><Trash2 size={15} aria-hidden="true" /></button> : null}
                </div>)}
                <button type="button" className="text-action" disabled={commandField.state.value.length >= 32} onClick={() => commandField.pushValue("")}><Plus size={14} aria-hidden="true" /> Add argument</button>
              </div>}</form.Field>
            </Disclosure>
          </div>)}
          <button type="button" className="secondary-button" disabled={checksField.state.value.length >= 64} onClick={() => checksField.pushValue({ id: "", command: [""] })}><Plus size={15} aria-hidden="true" /> Add check</button>
        </>}</form.Field>
        {error && mode.kind === "flow" ? <p className="auth-alert" role="alert">{error}</p> : null}
      </div>
    </section>
    <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
      {([dirty, submitting, canSubmit]) => mode.kind === "flow" ? <FlowActions mode={mode} submitting={submitting} canSubmit={canSubmit} label="Save checks and continue" />
        : <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved} onCancel={() => { form.reset({ checks: initial() }); setError(""); }} saveLabel="Save checks" />}
    </form.Subscribe>
  </form>;
}
