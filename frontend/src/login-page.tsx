import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useForm } from "@tanstack/react-form";
import { Eye, EyeOff, LogIn, RefreshCw } from "lucide-react";
import { announceSessionChange, clearWorkspaceCache, loginError, loginLocal, sessionQueryKey } from "./auth";
import { AuthFrame } from "./auth-frame";
import { TextField } from "./form-field";

export function LoginPage() {
  const queryClient = useQueryClient();
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState("");
  const form = useForm({
    defaultValues: { organizationSlug: "", username: "", password: "" },
    onSubmit: async ({ value }) => {
      setError("");
      try {
        const session = await loginLocal(value.organizationSlug.trim(), value.username.trim(), value.password);
        form.reset();
        await clearWorkspaceCache(queryClient);
        queryClient.setQueryData(sessionQueryKey, session);
        announceSessionChange();
      } catch (cause) {
        setError(loginError(cause));
      }
    },
  });

  return <AuthFrame>
    <div className="auth-heading"><p className="eyebrow">Welcome back</p><h2>Sign in to your workspace</h2><p>Use your organization’s local account to continue.</p></div>
    <form className="auth-form" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
      <div className="auth-fields">
        <form.Field name="organizationSlug" validators={{ onBlur: ({ value }) => value.trim() ? undefined : "Organization is required" }}>
          {(field) => <TextField autoFocus label="Organization" autoComplete="organization" placeholder="your-organization"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="username" validators={{ onBlur: ({ value }) => value.trim() ? undefined : "Username is required" }}>
          {(field) => <TextField label="Username" autoComplete="username" placeholder="Your username"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} />}
        </form.Field>
        <form.Field name="password" validators={{ onBlur: ({ value }) => value ? undefined : "Password is required" }}>
          {(field) => <TextField label="Password" type={showPassword ? "text" : "password"} autoComplete="current-password" placeholder="Enter your password"
            name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={field.handleChange}
            error={field.state.meta.errors.join(", ")} trailing={<button type="button" className="field-action" onClick={() => setShowPassword(!showPassword)}
              aria-label={showPassword ? "Hide password" : "Show password"}>{showPassword ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}</button>} />}
        </form.Field>
      </div>
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
        {([canSubmit, isSubmitting]) => <button className="primary-button auth-submit" type="submit" disabled={!canSubmit || isSubmitting}>{isSubmitting ? <RefreshCw className="spin" size={16} aria-hidden="true" /> : <LogIn size={16} aria-hidden="true" />}{isSubmitting ? "Signing in…" : "Sign in"}</button>}
      </form.Subscribe>
    </form>
    <p className="auth-help">Need access? Contact your organization administrator.</p>
  </AuthFrame>;
}
