import { useId, useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowLeft, RefreshCw, UserPlus } from "lucide-react";
import { emailProblem } from "../account";
import { OneTimeLink } from "../account-link";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import type { AccountLink } from "../gen/blaxsmith/api/v1/users_pb";
import { PageHeader, PageShell } from "../page";
import { assignableRoles, inviteUser, membersKey, userAdminError } from "../users";

export const Route = createFileRoute("/admin/users/new")({ component: InviteUser });

const roleHelp: Record<string, string> = {
  owner: "Full organization administration, including other owners.",
  admin: "Organization administration and member management, except owners.",
  member: "Works in the organization's projects and runs.",
  viewer: "Reads work and activity without changing it.",
};

function InviteUser() {
  const queryClient = useQueryClient();
  const roleId = useId();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const roles = assignableRoles(session.data);
  const [error, setError] = useState("");
  const [issued, setIssued] = useState<{ link: AccountLink; email: string } | null>(null);
  const form = useForm({
    defaultValues: { email: "", displayName: "", role: "member" },
    onSubmit: async ({ value }) => {
      setError("");
      const email = value.email.trim().toLowerCase();
      if (emailProblem(email) || value.displayName.trim().length > 160) {
        setError("Check the email and display name before inviting.");
        return;
      }
      try {
        const response = await inviteUser(email, value.displayName.trim(), value.role);
        await Promise.all([queryClient.invalidateQueries({ queryKey: membersKey(session.data?.organizationId || "") }),
          queryClient.invalidateQueries({ queryKey: ["workspace-members"] })]);
        if (response.link) setIssued({ link: response.link, email });
        else setError("The member was created, but no setup link was returned. Issue a new setup link from the Users list.");
      } catch (cause) {
        setError(userAdminError(cause));
      }
    },
  });


  return <PageShell>
    <PageHeader eyebrow="Administration / Users / New" title="Invite a user" description="Invite someone by email and get a one-time setup link to hand to them. No email is sent." />
    <Link to="/admin/users" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to users</Link>
    <div className="editor-layout">
      <section className="editor-card" aria-labelledby="invite-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserPlus size={18} aria-hidden="true" /></span><div><h2 id="invite-heading">{issued ? "Invitation created" : "Account details"}</h2><p>{issued ? `${issued.email} can now choose a password with this link.` : "They sign in with this email. It must not already belong to another account."}</p></div></div>
        {issued ? <>
          <OneTimeLink link={issued.link} recipient={issued.email} />
          <div className="editor-actions"><button type="button" className="secondary-button" onClick={() => { setIssued(null); form.reset(); }}>Invite another</button><Link to="/admin/users" className="primary-button">Done</Link></div>
        </> : <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
          <form.Field name="email" validators={{ onBlur: ({ value }) => emailProblem(value) }}>
            {(field) => <TextField autoFocus label="Email" type="email" name={field.name} autoComplete="off" placeholder="jordan@example.com" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          <form.Field name="displayName" validators={{ onBlur: ({ value }) => value.trim().length <= 160 ? undefined : "Use at most 160 characters." }}>
            {(field) => <TextField label="Display name (optional)" name={field.name} autoComplete="off" placeholder="Jordan Lee" required={false} value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
          </form.Field>
          <form.Field name="role">{(field) => <div className="form-field"><label htmlFor={roleId}>Role</label>
            <select id={roleId} name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}>
              {roles.map((role) => <option key={role} value={role}>{role[0].toUpperCase() + role.slice(1)}</option>)}</select>
            <span className="admin-note">{roleHelp[field.state.value]}</span></div>}</form.Field>
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
          <div className="editor-actions"><Link to="/admin/users" className="secondary-button">Cancel</Link><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
            {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <UserPlus size={15} aria-hidden="true" />}{submitting ? "Inviting…" : "Invite and create link"}</button>}
          </form.Subscribe></div>
        </form>}
      </section>
      <aside className="editor-note"><h2>How setup works</h2><p>The link works once and expires after 72 hours. Whoever opens it chooses the account's password, so share it only with the person you are inviting.</p><p>Only owners can invite another owner. The invitation is recorded in the audit log; the link itself is never stored or shown again.</p></aside>
    </div>
  </PageShell>;
}
