import { useId, useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowLeft, RefreshCw, UserPlus } from "lucide-react";
import { OneTimeLink } from "../account-link";
import { currentSession, sessionQueryKey } from "../auth";
import { TextField } from "../form-field";
import type { AccountLink } from "../gen/blaxsmith/api/v1/users_pb";
import { PageHeader, PageShell } from "../page";
import { assignableRoles, inviteUser, membersKey, userAdminError } from "../users";

export const Route = createFileRoute("/admin/users/new")({ component: InviteUser });

const usernamePattern = /^[a-z][a-z0-9._-]{2,63}$/;
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
  const [issued, setIssued] = useState<{ link: AccountLink; username: string } | null>(null);
  const form = useForm({
    defaultValues: { username: "", displayName: "", role: "member" },
    onSubmit: async ({ value }) => {
      setError("");
      const username = value.username.trim().toLowerCase();
      if (!usernamePattern.test(username) || value.displayName.trim().length > 160) {
        setError("Check the username and display name before inviting.");
        return;
      }
      try {
        const response = await inviteUser(username, value.displayName.trim(), value.role);
        await queryClient.invalidateQueries({ queryKey: membersKey(session.data?.organizationId || "") });
        if (response.link) setIssued({ link: response.link, username });
        else setError("The member was created, but no setup link was returned. Issue a new setup link from the Users list.");
      } catch (cause) {
        setError(userAdminError(cause));
      }
    },
  });


  return <PageShell>
    <PageHeader eyebrow="Administration / Users / New" title="Invite a user" description="Create a local account and get a one-time setup link to hand to them. No email is sent." />
    <Link to="/admin/users" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to users</Link>
    <div className="editor-layout">
      <section className="editor-card" aria-labelledby="invite-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserPlus size={18} aria-hidden="true" /></span><div><h2 id="invite-heading">{issued ? "Invitation created" : "Account details"}</h2><p>{issued ? `${issued.username} can now choose a password with this link.` : "The username is used to sign in and cannot be changed later."}</p></div></div>
        {issued ? <>
          <OneTimeLink link={issued.link} username={issued.username} />
          <div className="editor-actions"><button type="button" className="secondary-button" onClick={() => { setIssued(null); form.reset(); }}>Invite another</button><Link to="/admin/users" className="primary-button">Done</Link></div>
        </> : <form className="editor-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
          <form.Field name="username" validators={{ onBlur: ({ value }) => usernamePattern.test(value.trim().toLowerCase()) ? undefined : "Use 3–64 lowercase letters, numbers, dots, underscores, or hyphens; start with a letter." }}>
            {(field) => <TextField autoFocus label="Username" name={field.name} autoComplete="off" placeholder="jordan" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} error={field.state.meta.errors.join(", ")} />}
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
