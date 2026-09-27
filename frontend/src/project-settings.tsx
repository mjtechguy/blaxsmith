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
  projectVerificationQueryKey, setProjectSource, setProjectVerification, listProjectVerificationHistory,
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
          queryClient.invalidateQueries({ queryKey: ["verification-history", org, projectId] }),
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

const presetMode = (preset: string, category: string) => preset === "mvp"
  ? category === "build" ? "required" : ["test", "other"].includes(category) ? "advisory" : "off"
  : preset === "balanced" && ["review", "e2e", "security", "performance"].includes(category) ? "advisory" : "required";
const checkId = /^[a-z][a-z0-9_-]{0,63}$/;

export function VerificationEditor({ projectId, org, current, mode }: { projectId: string; org: string; current: ProjectVerification | null; mode: EditorMode }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [saved, setSaved] = useSaved();
  const [baseVersion, setBaseVersion] = useState(current?.version ?? 0n);
  const [preset, setPreset] = useState(current?.preset || (current ? "custom" : "balanced"));
  const initial = () => current?.checks.map((check) => ({ category: check.category || "other", mode: check.mode || "required", id: check.id, command: [...check.command], trustedPaths: check.trustedPaths.join(", ") })) || [];
  // Suggestions from .blaxsmith.json or detection prefill an empty policy; an
  // existing one is replaced only on request. Nothing is saved until the user confirms.
  const inspection = useRepositoryInspection(projectId);
  const suggested = inspection.data?.verification.map((c) => ({ category: "other", mode: "required", id: c.id, command: [...c.command], trustedPaths: "" })) ?? [];
  const [prefilled, setPrefilled] = useState(false);
  const form = useForm({
    defaultValues: { checks: initial() },
    onSubmit: async ({ value }) => {
      setError("");
      const checks = value.checks.map((check) => ({ category: check.category, mode: check.mode, id: check.id.trim(), command: check.command, trustedPaths: check.trustedPaths.split(",").map((path) => path.trim()).filter(Boolean) }));
      if (checks.length > 64 || new Set(checks.map((check) => check.id)).size !== checks.length ||
        checks.some((check) => !checkId.test(check.id) || check.command.length < 1 || check.command.length > 32 ||
          check.command.some((part) => !part || part.length > 4096 || part.includes("\0")))) {
        setError("Use 0–64 checks with unique lowercase IDs and 1–32 nonempty command arguments each.");
        return;
      }
      try {
        const response = await setProjectVerification(projectId, checks, baseVersion, preset);
        setBaseVersion(response.verification!.version);
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: projectVerificationQueryKey(org, projectId) }),
          queryClient.invalidateQueries({ queryKey: launchAvailabilityQueryKey(org, projectId) }),
          queryClient.invalidateQueries({ queryKey: ["verification-history", org, projectId] }),
        ]);
        form.reset({ checks: checks.map((check) => ({ ...check, trustedPaths: check.trustedPaths.join(", ") })) });
        setSaved(true);
        if (mode.kind === "flow") mode.next();
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        if (code === Code.Aborted) await queryClient.invalidateQueries({ queryKey: projectVerificationQueryKey(org, projectId) });
        setError(code === Code.InvalidArgument ? "Check IDs and command arguments are invalid. Use unique lowercase IDs and separate argv fields."
          : code === Code.Aborted ? "Checks changed elsewhere. Your draft is preserved. Cancel to load the current version, then reapply your changes."
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
  const applySuggested = () => { form.setFieldValue("checks", suggested); setPreset("custom"); setPrefilled(true); };

  return <form className="settings-form" onChangeCapture={() => setPreset("custom")} noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="checks-heading">
      <div className="editor-card-heading"><span className="project-symbol"><ShieldCheck size={18} aria-hidden="true" /></span><div><h2 id="checks-heading">Checks</h2>
        <p>{current ? `Version ${current.version.toString()}${current.updatedAt ? ` · updated ${new Date(current.updatedAt).toLocaleString()}` : ""}. ` : "Choose required, advisory, or off for each check. An empty policy explicitly selects no automated checks. "}Enter the executable and each argument separately; shell syntax is not parsed.</p></div></div>
      <div className="editor-form">
        <RepositorySuggestion projectId={projectId} />
        <div className="form-field"><label htmlFor="verification-preset">Check preset</label><select id="verification-preset" value={preset} onChange={(event) => {
          const selected = event.target.value; setPreset(selected);
          if (selected !== "custom") form.setFieldValue("checks", form.state.values.checks.map((check) => ({ ...check, mode: presetMode(selected, check.category) })));
        }}><option value="mvp">MVP</option><option value="balanced">Balanced</option><option value="thorough">Thorough</option><option value="custom">Custom</option></select></div>
        <p className="form-hint">Choose each check’s category first. MVP requires build checks, makes focused tests advisory and turns review, E2E, security and performance off. Balanced requires build and focused tests and makes the others advisory. Thorough requires every configured check. You can change every mode. Presets never add missing checks.</p>
        <p className="form-hint">Saving affects future runs. Existing runs retain their frozen policy and all historical results.</p>
        {prefilled ? <p className="notice" role="status">These checks were prefilled from the repository. Review them, then {mode.kind === "flow" ? "save and continue" : "save"} to confirm.</p> : null}
        {current && suggested.length && !prefilled ? <button type="button" className="secondary-button" onClick={applySuggested}>Use suggested checks</button> : null}
        <form.Field name="checks" mode="array">{(checksField) => <>
          {checksField.state.value.length === 0 ? <p className="notice">No automated checks selected. Save to confirm this policy for future runs.</p> : null}
          {checksField.state.value.map((check, checkIndex) => <div className="verification-check" key={checkIndex}>
            <div className="verification-check-heading"><strong>Check {checkIndex + 1}{check.id ? ` · ${check.id}` : ""}</strong><button type="button" className="text-action"  onClick={() => { setPreset("custom"); checksField.removeValue(checkIndex); }}><Trash2 size={14} aria-hidden="true" /> Remove</button></div>
            <form.Field name={`checks[${checkIndex}].id`} validators={{ onBlur: ({ value }) => checkId.test(value.trim()) ? undefined : "Use a lowercase ID starting with a letter, up to 64 characters." }}>
              {(field) => <TextField label="Check ID" name={field.name} autoComplete="off" placeholder="unit-tests" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
            </form.Field>
            <form.Field name={`checks[${checkIndex}].category`}>{(field) => <div className="form-field"><label htmlFor={`check-category-${checkIndex}`}>Category</label><select id={`check-category-${checkIndex}`} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)}><option value="other">Other</option><option value="build">Build / type check</option><option value="test">Focused test</option><option value="review">Independent review command</option><option value="e2e">Browser / E2E</option><option value="security">Security</option><option value="performance">Performance</option></select></div>}</form.Field>
            <form.Field name={`checks[${checkIndex}].mode`}>{(field) => <div className="form-field"><label htmlFor={`check-mode-${checkIndex}`}>Enforcement</label><select id={`check-mode-${checkIndex}`} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}><option value="required">Required — must pass</option><option value="advisory">Advisory — record findings</option><option value="off">Off — do not run</option></select></div>}</form.Field>
            <form.Field name={`checks[${checkIndex}].trustedPaths`}>
              {(field) => <TextField required={false} label="Protected check paths" name={field.name} autoComplete="off" placeholder="tests, scripts/verify.sh, package.json" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}
            </form.Field>
            <p className="form-hint">Comma-separated repository files or directories. These must match the run’s original revision, so candidate changes cannot weaken the checks. Include test scripts and configuration used by this command.</p>
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
          <button type="button" className="secondary-button" disabled={checksField.state.value.length >= 64} onClick={() => { setPreset("custom"); checksField.pushValue({ category: "other", mode: "required", id: "", command: [""], trustedPaths: "" }); }}><Plus size={15} aria-hidden="true" /> Add check</button>
        </>}</form.Field>
        {error && mode.kind === "flow" ? <p className="auth-alert" role="alert">{error}</p> : null}
      </div>
    </section>
    <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
      {([dirty, submitting, canSubmit]) => mode.kind === "flow" ? <FlowActions mode={mode} submitting={submitting} canSubmit={canSubmit} label="Save checks and continue" />
        : <GuardedSaveBar dirty={dirty || !current || preset !== (current.preset || "custom")} saving={submitting} canSave={canSubmit} error={error} saved={saved} onCancel={() => { form.reset({ checks: initial() }); setBaseVersion(current?.version ?? 0n); setPreset(current?.preset || "custom"); setError(""); }} saveLabel="Save checks" />}
    </form.Subscribe>
    {current ? <VerificationHistory projectId={projectId} org={org} /> : null}
  </form>;
}

function VerificationHistory({ projectId, org }: { projectId: string; org: string }) {
 const [before, setBefore] = useState(0n);
 const history = useQuery({ queryKey: ["verification-history", org, projectId, before.toString()], queryFn: ({ signal }) => listProjectVerificationHistory(projectId, before, signal) });
 return <details className="editor-card"><summary>Check policy history</summary>
  {history.isPending ? <p>Loading revisions…</p> : history.isError ? <p role="alert">History could not be loaded. <button type="button" onClick={() => void history.refetch()}>Retry</button></p> : history.data.revisions.map((revision) => <section key={revision.version.toString()}>
   <h3>Version {revision.version.toString()} · {revision.preset || "custom"}</h3><p>{new Date(revision.updatedAt).toLocaleString()}</p>
   {revision.checks.length ? <ul>{revision.checks.map((check) => <li key={check.id}>{check.id} · {check.category || "other"} · {check.mode || "required"}<pre>{JSON.stringify(check.command)}</pre><p>Protected paths: {check.trustedPaths.join(", ") || "none"}</p></li>)}</ul> : <p>No automated checks selected.</p>}
  </section>)}
  {history.data?.nextBeforeVersion ? <button type="button" className="secondary-button" onClick={() => setBefore(history.data.nextBeforeVersion)}>Older revisions</button> : null}
  {before ? <button type="button" className="secondary-button" onClick={() => setBefore(0n)}>Newest revisions</button> : null}
 </details>;
}
