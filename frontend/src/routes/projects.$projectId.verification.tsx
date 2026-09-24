import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import type { ProjectVerification } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getProject, getProjectVerification, launchAvailabilityQueryKey, projectVerificationQueryKey, setProjectVerification } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/verification")({ component: VerificationSettings });
const checkId = /^[a-z][a-z0-9_-]{0,63}$/;

function VerificationSettings() {
  const { projectId } = Route.useParams();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: Boolean(org && project.data?.project), queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const mayEdit = session.data?.role === "owner" || session.data?.role === "admin";

  return <PageShell>
    <PageHeader eyebrow="Project / Verification" title="Verification checks" description={project.data?.project ? `Define the checks for ${project.data.project.name}.` : "Define the checks for this project."} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to project</Link>
    {project.isPending || (project.isSuccess && verification.isPending) ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading verification</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {verification.isError ? <div className="state-panel" role="alert"><h2>Verification unavailable</h2><p>Checks could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void verification.refetch()}>Try again</button></div> : null}
    {project.data?.project && verification.isSuccess && !mayEdit ? <div className="state-panel" role="note"><h2>Checks are read-only</h2><p>Only organization owners and admins can change verification.</p></div> : null}
    {project.data?.project && verification.isSuccess && mayEdit ? <VerificationEditor projectId={projectId} org={org} current={verification.data} /> : null}
  </PageShell>;
}

function VerificationEditor({ projectId, org, current }: { projectId: string; org: string; current: ProjectVerification | null }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { checks: current?.checks.map((check) => ({ id: check.id, command: [...check.command] })) || [{ id: "", command: [""] }] },
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
        await navigate({ to: "/projects/$projectId", params: { projectId } });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.InvalidArgument ? "Check IDs and command arguments are invalid. Use unique lowercase IDs and separate argv fields."
          : code === Code.PermissionDenied ? "Your session cannot change verification."
            : "Verification could not be saved. Please try again.");
      }
    },
  });

  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="checks-heading">
      <div className="editor-card-heading"><span className="project-symbol"><ShieldCheck size={18} aria-hidden="true" /></span><div><h2 id="checks-heading">Checks</h2><p>{current ? `Version ${current.version.toString()} · Updated ${new Date(current.updatedAt).toLocaleString()}` : "At least one check is required to launch a run."}</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="checks" mode="array">{(checksField) => <>
          {checksField.state.value.map((_, checkIndex) => <div className="verification-check" key={checkIndex}>
            <div className="verification-check-heading"><strong>Check {checkIndex + 1}</strong><button type="button" className="text-action" disabled={checksField.state.value.length === 1} onClick={() => checksField.removeValue(checkIndex)}><Trash2 size={14} aria-hidden="true" /> Remove</button></div>
            <form.Field name={`checks[${checkIndex}].id`} validators={{ onBlur: ({ value }) => checkId.test(value.trim()) ? undefined : "Use a lowercase ID starting with a letter, up to 64 characters." }}>
              {(field) => <TextField label="Check ID" name={field.name} autoComplete="off" placeholder="unit-tests" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
            </form.Field>
            <form.Field name={`checks[${checkIndex}].command`} mode="array">{(commandField) => <div className="verification-arguments">
              <span>Command argv</span>
              {commandField.state.value.map((_, argIndex) => <div className="verification-argument" key={argIndex}>
                <form.Field name={`checks[${checkIndex}].command[${argIndex}]`}>
                  {(field) => <TextField label={argIndex === 0 ? "Executable" : `Argument ${argIndex}`} name={field.name} autoComplete="off" placeholder={argIndex === 0 ? "npm" : "test"} value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
                </form.Field>
                {argIndex > 0 ? <button type="button" className="secondary-button" aria-label={`Remove argument ${argIndex} from check ${checkIndex + 1}`} onClick={() => commandField.removeValue(argIndex)}><Trash2 size={15} aria-hidden="true" /></button> : null}
              </div>)}
              <button type="button" className="text-action" disabled={commandField.state.value.length >= 32} onClick={() => commandField.pushValue("")}><Plus size={14} aria-hidden="true" /> Add argument</button>
            </div>}</form.Field>
          </div>)}
          <button type="button" className="secondary-button" disabled={checksField.state.value.length >= 64} onClick={() => checksField.pushValue({ id: "", command: [""] })}><Plus size={15} aria-hidden="true" /> Add check</button>
        </>}</form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions"><Link to="/projects/$projectId" params={{ projectId }} className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <ShieldCheck size={15} aria-hidden="true" />}{submitting ? "Saving…" : "Save checks"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    <aside className="editor-note"><h2>Exact command arguments</h2><p>Enter the executable and each argument in its own field. Shell syntax is not parsed. Recipe-required check IDs must be present here before a run can launch.</p></aside>
  </div>;
}
