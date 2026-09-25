import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Eye, EyeOff, KeyRound, LogIn, RefreshCw } from "lucide-react";
import { emailProblem, passwordBytes } from "../account";
import { sessionQueryKey } from "../auth";
import { AuthFrame } from "../auth-frame";
import { TextField } from "../form-field";
import { completeAccountLink, getAccountLink } from "../users";

export const Route = createFileRoute("/setup/$token")({ component: AccountSetup });

function AccountSetup() {
  const { token } = Route.useParams();
  const queryClient = useQueryClient();
  const link = useQuery({ queryKey: ["account-link", token], queryFn: ({ signal }) => getAccountLink(token, signal), retry: false, staleTime: Infinity });
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState<{ organizationSlug: string; email: string } | null>(null);
  // An account from before emails has none; setup then asks for one.
  const askEmail = link.data ? !link.data.email : false;
  const form = useForm({
    defaultValues: { displayName: "", email: "", password: "", confirm: "" },
    onSubmit: async ({ value }) => {
      setError("");
      if (passwordBytes(value.password) < 12 || passwordBytes(value.password) > 1024 || value.password !== value.confirm) {
        setError("Use a password of at least 12 characters and enter it twice.");
        return;
      }
      if (value.displayName.trim().length > 160 || (askEmail && emailProblem(value.email))) {
        setError(askEmail && emailProblem(value.email) ? "Enter the email you will sign in with." : "Use a display name of at most 160 characters.");
        return;
      }
      try {
        const response = await completeAccountLink(token, value.password, value.displayName.trim(), askEmail ? value.email.trim() : "");
        form.reset();
        queryClient.removeQueries({ queryKey: ["account-link", token] });
        // Any session this browser held for the account has just been revoked.
        await queryClient.invalidateQueries({ queryKey: sessionQueryKey });
        setDone({ organizationSlug: response.organizationSlug, email: response.email });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.NotFound ? "This link has already been used, was replaced, or has expired. Ask your administrator for a new one."
          : code === Code.AlreadyExists ? "That email is already used by another account."
          : code === Code.InvalidArgument ? ConnectError.from(cause).rawMessage.includes("email") ? "This email address can't be used. Check it or try a different one." : "Use a password of 12–1024 characters."
            : code === Code.PermissionDenied ? "This page's security check failed. Reload the page and try again."
              : "Your password could not be saved. Please try again.");
      }
    },
  });

  if (done) return <AuthFrame>
    <div className="auth-heading" role="status"><p className="eyebrow">All set</p><h1>Your password is saved</h1><p>Sign in to <strong>{done.organizationSlug}</strong> with <strong>{done.email}</strong>. Any earlier sessions for this account were signed out.</p></div>
    <Link className="primary-button auth-submit" to="/login" search={{ next: "/" }}><LogIn size={16} aria-hidden="true" /> Continue to sign in</Link>
  </AuthFrame>;
  if (link.isPending) return <AuthFrame><div className="auth-heading" role="status"><h1>Checking your link</h1><p>One moment.</p></div></AuthFrame>;
  if (link.isError) {
    const missing = ConnectError.from(link.error).code === Code.NotFound;
    return <AuthFrame><div className="auth-heading" role="alert"><p className="eyebrow">{missing ? "Link unavailable" : "Connection needed"}</p>
      <h1>{missing ? "This link can't be used" : "We couldn't check this link"}</h1>
      <p>{missing ? "Setup and reset links work once and expire. This one was already used, replaced by a newer link, or has expired. Ask your organization administrator for a new link." : "The identity service could not be reached. Try again in a moment."}</p></div>
      {missing ? <Link className="secondary-button" to="/login" search={{ next: "/" }}>Go to sign in</Link>
        : <button className="secondary-button" type="button" onClick={() => void link.refetch()}><RefreshCw size={16} aria-hidden="true" /> Try again</button>}
    </AuthFrame>;
  }
  const info = link.data;
  const reset = info.purpose === "reset";
  return <AuthFrame title={reset ? "Reset password" : "Set up your account"}>
    <div className="auth-heading"><p className="eyebrow">{reset ? "Password reset" : "Account setup"}</p>
      <h1>{reset ? "Choose a new password" : `Welcome${info.displayName ? `, ${info.displayName}` : ""}`}</h1>
      <p>{reset ? "Set a new password for your account" : "Choose a password for your account"} in <strong>{info.organizationName}</strong>. This link expires {new Date(info.expiresAt).toLocaleString()}.</p></div>
    <form className="auth-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <div className="auth-fields">
        {askEmail ? <form.Field name="email" validators={{ onBlur: ({ value }) => emailProblem(value) }}>
          {(field) => <TextField autoFocus label="Email" type="email" autoComplete="username" placeholder="you@example.com"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")} />}
        </form.Field> : <TextField label="Email" type="email" name="username" autoComplete="username" placeholder="" value={info.email} readOnly
          onChange={() => undefined} onBlur={() => undefined} />}
        {reset ? null : <form.Field name="displayName" validators={{ onBlur: ({ value }) => value.trim().length <= 160 ? undefined : "Use at most 160 characters." }}>
          {(field) => <TextField label="Display name (optional)" autoComplete="name" placeholder={info.displayName || "How your team sees you"} required={false}
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")} />}
        </form.Field>}
        <form.Field name="password" validators={{ onBlur: ({ value }) => passwordBytes(value) >= 12 ? passwordBytes(value) <= 1024 ? undefined : "Use at most 1024 characters." : "Use at least 12 characters." }}>
          {(field) => <TextField autoFocus={!askEmail} label="New password" type={showPassword ? "text" : "password"} autoComplete="new-password" placeholder="At least 12 characters"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")}
            trailing={<button type="button" className="field-action" onClick={() => setShowPassword(!showPassword)} aria-label={showPassword ? "Hide password" : "Show password"}>
              {showPassword ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}</button>} />}
        </form.Field>
        <form.Field name="confirm" validators={{ onChangeListenTo: ["password"], onBlur: ({ value, fieldApi }) => value === fieldApi.form.getFieldValue("password") ? undefined : "Passwords do not match." }}>
          {(field) => <TextField label="Confirm password" type={showPassword ? "text" : "password"} autoComplete="new-password" placeholder="Enter it again"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange} error={field.state.meta.errors.join(", ")} />}
        </form.Field>
      </div>
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
        {([canSubmit, isSubmitting]) => <button className="primary-button auth-submit" type="submit" disabled={!canSubmit || isSubmitting}>{isSubmitting ? <RefreshCw className="spin" size={16} aria-hidden="true" /> : <KeyRound size={16} aria-hidden="true" />}{isSubmitting ? "Saving…" : reset ? "Set new password" : "Set password"}</button>}
      </form.Subscribe>
    </form>
    <p className="auth-help">Saving signs this account out of any other sessions. Didn't expect this link? Close this page and tell your administrator.</p>
  </AuthFrame>;
}
