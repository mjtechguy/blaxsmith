import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Eye, EyeOff, KeyRound, LogIn, RefreshCw } from "lucide-react";
import { sessionQueryKey } from "../auth";
import { AuthFrame } from "../auth-frame";
import { TextField } from "../form-field";
import { completeAccountLink, getAccountLink } from "../users";

export const Route = createFileRoute("/setup/$token")({ component: AccountSetup });

// Mirrors the server: 12–1024 bytes of UTF-8.
const passwordBytes = (value: string) => new TextEncoder().encode(value).length;

function AccountSetup() {
  const { token } = Route.useParams();
  const queryClient = useQueryClient();
  const link = useQuery({ queryKey: ["account-link", token], queryFn: ({ signal }) => getAccountLink(token, signal), retry: false, staleTime: Infinity });
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState<{ organizationSlug: string; username: string } | null>(null);
  const form = useForm({
    defaultValues: { password: "", confirm: "" },
    onSubmit: async ({ value }) => {
      setError("");
      if (passwordBytes(value.password) < 12 || passwordBytes(value.password) > 1024 || value.password !== value.confirm) {
        setError("Use a password of at least 12 characters and enter it twice.");
        return;
      }
      try {
        const response = await completeAccountLink(token, value.password);
        form.reset();
        queryClient.removeQueries({ queryKey: ["account-link", token] });
        // Any session this browser held for the account has just been revoked.
        await queryClient.invalidateQueries({ queryKey: sessionQueryKey });
        setDone({ organizationSlug: response.organizationSlug, username: response.username });
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.NotFound ? "This link has already been used, was replaced, or has expired. Ask your administrator for a new one."
          : code === Code.InvalidArgument ? "Use a password of 12–1024 characters."
            : code === Code.PermissionDenied ? "This page's security check failed. Reload the page and try again."
              : "Your password could not be saved. Please try again.");
      }
    },
  });

  if (done) return <AuthFrame>
    <div className="auth-heading" role="status"><p className="eyebrow">All set</p><h2>Your password is saved</h2><p>Sign in to <strong>{done.organizationSlug}</strong> as <strong>{done.username}</strong>. Any earlier sessions for this account were signed out.</p></div>
    <Link className="primary-button auth-submit" to="/login" search={{ next: "/" }}><LogIn size={16} aria-hidden="true" /> Continue to sign in</Link>
  </AuthFrame>;
  if (link.isPending) return <AuthFrame><div className="auth-heading" role="status"><h2>Checking your link</h2><p>One moment.</p></div></AuthFrame>;
  if (link.isError) {
    const missing = ConnectError.from(link.error).code === Code.NotFound;
    return <AuthFrame><div className="auth-heading" role="alert"><p className="eyebrow">{missing ? "Link unavailable" : "Connection needed"}</p>
      <h2>{missing ? "This link can't be used" : "We couldn't check this link"}</h2>
      <p>{missing ? "Setup and reset links work once and expire. This one was already used, replaced by a newer link, or has expired. Ask your organization administrator for a new link." : "The identity service could not be reached. Try again in a moment."}</p></div>
      {missing ? <Link className="secondary-button" to="/login" search={{ next: "/" }}>Go to sign in</Link>
        : <button className="secondary-button" type="button" onClick={() => void link.refetch()}><RefreshCw size={16} aria-hidden="true" /> Try again</button>}
    </AuthFrame>;
  }
  const info = link.data;
  const reset = info.purpose === "reset";
  return <AuthFrame>
    <div className="auth-heading"><p className="eyebrow">{reset ? "Password reset" : "Account setup"}</p>
      <h2>{reset ? "Choose a new password" : `Welcome${info.displayName ? `, ${info.displayName}` : ""}`}</h2>
      <p>{reset ? "Set a new password for" : "Choose a password for"} <strong>{info.username}</strong> in <strong>{info.organizationName}</strong>. This link expires {new Date(info.expiresAt).toLocaleString()}.</p></div>
    <form className="auth-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <input type="text" name="username" autoComplete="username" value={info.username} readOnly hidden />
      <div className="auth-fields">
        <form.Field name="password" validators={{ onBlur: ({ value }) => passwordBytes(value) >= 12 ? passwordBytes(value) <= 1024 ? undefined : "Use at most 1024 characters." : "Use at least 12 characters." }}>
          {(field) => <TextField autoFocus label="New password" type={showPassword ? "text" : "password"} autoComplete="new-password" placeholder="At least 12 characters"
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
