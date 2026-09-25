import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQueryClient } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { AtSign, Eye, EyeOff, LogOut, RefreshCw } from "lucide-react";
import { accountError, emailProblem, updateMyProfile } from "../account";
import { announceSessionChange, clearWorkspaceCache, logout, sessionQueryKey } from "../auth";
import { AuthFrame } from "../auth-frame";
import { TextField } from "../form-field";

export const Route = createFileRoute("/me/email")({ component: SetEmail });

// The one step a session signed in with a legacy username must finish: until
// an email is saved the server refuses everything else, so the root layout
// routes here instead of the workspace.
function SetEmail() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { email: "", currentPassword: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const problem = emailProblem(value.email);
      if (problem || !value.currentPassword) {
        setError(problem ?? "Enter the password you just signed in with.");
        return;
      }
      try {
        await updateMyProfile({ email: value.email.trim(), currentPassword: value.currentPassword });
        form.reset();
        await clearWorkspaceCache(queryClient);
        await queryClient.invalidateQueries({ queryKey: sessionQueryKey });
        announceSessionChange();
        await navigate({ to: "/", replace: true });
      } catch (cause) {
        setError(accountError(cause, "Your email could not be saved. Please try again."));
      }
    },
  });
  const signOut = async () => {
    try { await logout(); } finally {
      queryClient.setQueryData(sessionQueryKey, null);
      announceSessionChange();
    }
  };

  return <AuthFrame title="Set your email">
    <div className="auth-heading"><p className="eyebrow">One more step</p><h1>Set your email</h1>
      <p>Accounts now sign in with an email instead of a username. Your username worked this once; add the email you will use from now on. Other sessions for this account are signed out.</p></div>
    <form className="auth-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <div className="auth-fields">
        <form.Field name="email" validators={{ onBlur: ({ value }) => emailProblem(value) }}>
          {(field) => <TextField autoFocus label="Email" type="email" autoComplete="email" placeholder="you@example.com"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="currentPassword" validators={{ onBlur: ({ value }) => value ? undefined : "Password is required" }}>
          {(field) => <TextField label="Current password" type={showPassword ? "text" : "password"} autoComplete="current-password" placeholder="The password you just used"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")}
            trailing={<button type="button" className="field-action" onClick={() => setShowPassword(!showPassword)} aria-label={showPassword ? "Hide password" : "Show password"}>
              {showPassword ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}</button>} />}
        </form.Field>
      </div>
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
        {([canSubmit, isSubmitting]) => <button className="primary-button auth-submit" type="submit" disabled={!canSubmit || isSubmitting}>{isSubmitting ? <RefreshCw className="spin" size={16} aria-hidden="true" /> : <AtSign size={16} aria-hidden="true" />}{isSubmitting ? "Saving…" : "Save email and continue"}</button>}
      </form.Subscribe>
    </form>
    <button type="button" className="secondary-button" onClick={() => void signOut()}><LogOut size={16} aria-hidden="true" /> Sign out instead</button>
    <p className="auth-help">This session can only set an email until you do. The email is not verified yet; no mail is sent.</p>
  </AuthFrame>;
}
