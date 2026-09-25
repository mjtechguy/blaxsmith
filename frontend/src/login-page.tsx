import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useForm } from "@tanstack/react-form";
import { Eye, EyeOff, LogIn, RefreshCw } from "lucide-react";
import { announceSessionChange, clearWorkspaceCache, loginError, loginLocal, needsOrganization, sessionQueryKey } from "./auth";
import { AuthFrame } from "./auth-frame";
import { TextField } from "./form-field";

export function LoginPage() {
  const queryClient = useQueryClient();
  const [showPassword, setShowPassword] = useState(false);
  // Asked for only when the account belongs to several organizations.
  const [askOrganization, setAskOrganization] = useState(false);
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { organizationSlug: "", email: "", password: "" },
    onSubmit: async ({ value }) => {
      setError("");
      try {
        const session = await loginLocal(askOrganization ? value.organizationSlug.trim() : "", value.email.trim(), value.password);
        form.reset();
        await clearWorkspaceCache(queryClient);
        queryClient.setQueryData(sessionQueryKey, session);
        announceSessionChange();
      } catch (cause) {
        if (needsOrganization(cause)) setAskOrganization(true);
        setError(loginError(cause));
      }
    },
  });

  return <AuthFrame title="Sign in">
    <div className="auth-heading"><p className="eyebrow">Welcome back</p><h1>Sign in to your workspace</h1><p>Use the email and password for your account.</p></div>
    <form className="auth-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <div className="auth-fields">
        <form.Field name="email" validators={{ onBlur: ({ value }) => value.trim() ? undefined : "Email is required" }}>
          {(field) => <TextField autoFocus label="Email" type="email" autoComplete="username" placeholder="you@example.com"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="password" validators={{ onBlur: ({ value }) => value ? undefined : "Password is required" }}>
          {(field) => <TextField label="Password" type={showPassword ? "text" : "password"} autoComplete="current-password" placeholder="Enter your password"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} trailing={<button type="button" className="field-action" onClick={() => setShowPassword(!showPassword)}
              aria-label={showPassword ? "Hide password" : "Show password"}>{showPassword ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}</button>} />}
        </form.Field>
        {askOrganization ? <form.Field name="organizationSlug" validators={{ onBlur: ({ value }) => value.trim() ? undefined : "Organization is required" }}>
          {(field) => <TextField autoFocus label="Organization" autoComplete="organization" placeholder="your-organization"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} />}
        </form.Field> : null}
      </div>
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
        {([canSubmit, isSubmitting]) => <button className="primary-button auth-submit" type="submit" disabled={!canSubmit || isSubmitting}>{isSubmitting ? <RefreshCw className="spin" size={16} aria-hidden="true" /> : <LogIn size={16} aria-hidden="true" />}{isSubmitting ? "Signing in…" : "Sign in"}</button>}
      </form.Subscribe>
    </form>
    <p className="auth-help">Need access? Contact your organization administrator. Signing in for the first time since accounts moved to email? Enter your old username once; you will be asked for your email next.</p>
  </AuthFrame>;
}
