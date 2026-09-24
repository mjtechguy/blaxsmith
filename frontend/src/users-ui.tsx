// Member actions shared by the Users table (bulk) and a user's detail page.
// Every action is confirmed in the app dialog; the server rechecks role rules.
import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { emailProblem, personLabel } from "./account";
import { OneTimeLink } from "./account-link";
import type { AccountLink, OrgMember } from "./gen/blaxsmith/api/v1/users_pb";
import { issueResetLink, revokeUserSessions, setUserEmail, setUserEnabled, setUserRole, userAdminError } from "./users";
import type { MemberRole } from "./users";

export type MemberAction = { kind: "role" | "disable" | "enable" | "reset" | "revoke" | "email"; member: OrgMember };
export const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };
export const memberStatusBadge: Record<string, string> = { active: "state-succeeded", invited: "state-waiting", disabled: "state-failed" };
const titles: Record<MemberAction["kind"], string> = { role: "Change role", disable: "Disable member", enable: "Enable member", reset: "Issue reset link", revoke: "Sign out everywhere", email: "Change email" };

const invalidate = (queryClient: ReturnType<typeof useQueryClient>) => Promise.all([
  queryClient.invalidateQueries({ queryKey: ["org-members"] }), queryClient.invalidateQueries({ queryKey: ["workspace-members"] }),
]);

export function MemberActionDialog({ action, roles, onClose }: { action: MemberAction; roles: MemberRole[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const dialog = useRef<HTMLDialogElement>(null);
  const [role, setRole] = useState(action.member.role);
  const [email, setEmail] = useState(action.member.email);
  const emailError = action.kind === "email" ? emailProblem(email) : undefined;
  const [issued, setIssued] = useState<AccountLink | null>(null);
  const [error, setError] = useState("");
  const act = useMutation({
    mutationFn: async () => {
      const id = action.member.principalId;
      if (action.kind === "role") return setUserRole(id, role);
      if (action.kind === "disable" || action.kind === "enable") return setUserEnabled(id, action.kind === "enable");
      if (action.kind === "revoke") return revokeUserSessions(id);
      if (action.kind === "email") return setUserEmail(id, email.trim());
      return issueResetLink(id);
    },
    onSuccess: async (response) => {
      if (action.kind === "reset" && response && "link" in response && response.link) setIssued(response.link);
      else onClose();
      await invalidate(queryClient);
    },
    onError: (cause) => { setError(userAdminError(cause)); void invalidate(queryClient); },
  });
  useEffect(() => { dialog.current?.showModal(); }, []);
  const busy = act.isPending;
  const m = action.member;
  const name = personLabel(m);
  return <dialog ref={dialog} className="review-confirm" aria-labelledby="member-confirm-title" onCancel={(event) => { if (busy) event.preventDefault(); }} onClose={() => { if (!busy) onClose(); }}>
    <h3 id="member-confirm-title">{issued ? (issued.purpose === "reset" ? "Reset link ready" : "Setup link ready") : titles[action.kind]}</h3>
    {issued ? <OneTimeLink link={issued} recipient={name} /> : <>
      {action.kind === "role" ? <>
        <p>Choose a new role for <strong>{name}</strong>. Their current sessions end so the new role applies immediately.</p>
        <div className="form-field"><label htmlFor="member-role">Role</label>
          <select id="member-role" value={role} onChange={(event) => setRole(event.target.value)}>{roles.map((r) => <option key={r} value={r}>{roleLabel[r]}</option>)}</select></div>
      </> : null}
      {action.kind === "email" ? <>
        <p>Set the email <strong>{name}</strong> signs in with. It must not be used by another account, starts unverified, and their current sessions end.</p>
        <div className="form-field"><label htmlFor="member-email">Email</label>
          <input id="member-email" type="email" autoComplete="off" value={email} onChange={(event) => setEmail(event.target.value)} aria-invalid={emailError && email ? true : undefined} />
          {emailError && email ? <span className="form-field-error">{emailError}</span> : null}</div>
      </> : null}
      {action.kind === "disable" ? <p>Disable <strong>{name}</strong>? They are signed out now, cannot sign in, and any open setup or reset link stops working. You can enable them again later.</p> : null}
      {action.kind === "enable" ? <p>Enable <strong>{name}</strong>? They can sign in again with their existing password and keep the role <strong>{roleLabel[m.role]}</strong>.</p> : null}
      {action.kind === "reset" ? <p>Issue a one-time {m.status === "invited" ? "setup" : "password reset"} link for <strong>{name}</strong>? Any earlier open link stops working. Using it sets a new password and signs them out everywhere.</p> : null}
      {action.kind === "revoke" ? <p>Sign <strong>{name}</strong> out of all {m.activeSessions} active session{m.activeSessions === 1 ? "" : "s"}? Their password is unchanged.</p> : null}
      <p className="admin-note">This change is recorded in the audit log.</p>
    </>}
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <div className="review-confirm-actions">
      {issued ? <button type="button" className="primary-button" onClick={onClose}>Done</button> : <>
        <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>Cancel</button>
        <button type="button" className={`primary-button${action.kind === "disable" || action.kind === "revoke" ? " danger-button" : ""}`}
          disabled={busy || (action.kind === "role" && role === m.role) || (action.kind === "email" && (Boolean(emailError) || email.trim().toLowerCase() === m.email))} onClick={() => act.mutate()}>{busy ? "Working…" : titles[action.kind]}</button>
      </>}
    </div>
  </dialog>;
}

// Bulk disable or sign-out across selected members; reports partial failure per member.
export function BulkMemberDialog({ kind, members, onClose }: { kind: "disable" | "revoke"; members: OrgMember[]; onClose: (done: boolean) => void }) {
  const queryClient = useQueryClient();
  const dialog = useRef<HTMLDialogElement>(null);
  const [result, setResult] = useState("");
  const act = useMutation({
    mutationFn: async () => {
      const results = await Promise.allSettled(members.map((m) => kind === "disable" ? setUserEnabled(m.principalId, false) : revokeUserSessions(m.principalId)));
      return results.map((r, i) => ({ member: members[i], error: r.status === "rejected" ? userAdminError(r.reason) : "" }));
    },
    onSuccess: async (results) => {
      await invalidate(queryClient);
      const failed = results.filter((r) => r.error);
      if (!failed.length) { onClose(true); return; }
      setResult(`${results.length - failed.length} of ${results.length} done. Not changed: ${failed.map((f) => `${personLabel(f.member)} (${f.error})`).join("; ")}`);
    },
  });
  useEffect(() => { dialog.current?.showModal(); }, []);
  const names = members.map((m) => personLabel(m)).join(", ");
  return <dialog ref={dialog} className="review-confirm" aria-labelledby="bulk-member-title" onCancel={(event) => { if (act.isPending) event.preventDefault(); }} onClose={() => { if (!act.isPending) onClose(Boolean(result)); }}>
    <h3 id="bulk-member-title">{kind === "disable" ? `Disable ${members.length} members` : `Sign out ${members.length} members`}</h3>
    <p>{kind === "disable" ? <>Disable <strong>{names}</strong>? They are signed out, cannot sign in, and open setup or reset links stop working.</> : <>Sign <strong>{names}</strong> out of every session? Passwords are unchanged.</>}</p>
    <p className="admin-note">Each change is checked by the server and recorded in the audit log.</p>
    {result ? <p className="auth-alert" role="alert">{result}</p> : null}
    <div className="review-confirm-actions">
      {result ? <button type="button" className="primary-button" onClick={() => onClose(true)}>Done</button> : <>
        <button type="button" className="secondary-button" disabled={act.isPending} onClick={() => onClose(false)}>Cancel</button>
        <button type="button" className="primary-button danger-button" disabled={act.isPending} onClick={() => act.mutate()}>{act.isPending ? "Working…" : kind === "disable" ? "Disable members" : "Sign out members"}</button>
      </>}
    </div>
  </dialog>;
}
