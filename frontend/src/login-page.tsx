import { useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useForm } from "@tanstack/react-form";
import { Eye, EyeOff, Hammer, LogIn, RefreshCw } from "lucide-react";
import { announceSessionChange, clearWorkspaceCache, loginError, loginLocal, sessionQueryKey } from "./auth";

export function AuthFrame({ children }: { children: ReactNode }) {
  return <main className="auth-frame" id="main-content">
    <aside className="auth-story" aria-label="About Blaxsmith">
      <div className="auth-grid" aria-hidden="true" />
      <div className="auth-story-content">
        <div className="auth-brand"><span className="brand-mark"><Hammer size={18} aria-hidden="true" /></span><strong>Blaxsmith</strong></div>
        <div className="auth-message"><p className="auth-kicker">Engineering work, in one place</p><h1>Shape the work. Guide the agents. Review the result.</h1><p>Plan, build, verify, and hand off with a clear record of every decision.</p></div>
        <p className="auth-story-footer">A workspace for human-led agent teams.</p>
      </div>
    </aside>
    <section className="auth-main"><div className="auth-content">
      <div className="auth-mobile-brand"><span className="brand-mark"><Hammer size={17} aria-hidden="true" /></span><strong>Blaxsmith</strong></div>
      {children}
    </div></section>
  </main>;
}

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
          {(field) => <label className="auth-field">Organization
            <input autoFocus autoComplete="organization" name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={(event) => field.handleChange(event.target.value)} required placeholder="your-organization" aria-invalid={field.state.meta.errors.length > 0} />
            {field.state.meta.errors.length > 0 ? <span className="auth-field-error">{field.state.meta.errors.join(", ")}</span> : null}
          </label>}
        </form.Field>
        <form.Field name="username" validators={{ onBlur: ({ value }) => value.trim() ? undefined : "Username is required" }}>
          {(field) => <label className="auth-field">Username
            <input autoComplete="username" name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={(event) => field.handleChange(event.target.value)} required placeholder="Your username" aria-invalid={field.state.meta.errors.length > 0} />
            {field.state.meta.errors.length > 0 ? <span className="auth-field-error">{field.state.meta.errors.join(", ")}</span> : null}
          </label>}
        </form.Field>
        <form.Field name="password" validators={{ onBlur: ({ value }) => value ? undefined : "Password is required" }}>
          {(field) => <label className="auth-field">Password
            <span className="auth-password"><input type={showPassword ? "text" : "password"} autoComplete="current-password" name={field.name} value={field.state.value} onBlur={field.handleBlur} onChange={(event) => field.handleChange(event.target.value)} required placeholder="Enter your password" aria-invalid={field.state.meta.errors.length > 0} />
              <button type="button" className="auth-reveal" onClick={() => setShowPassword(!showPassword)} aria-label={showPassword ? "Hide password" : "Show password"}>{showPassword ? <EyeOff size={17} aria-hidden="true" /> : <Eye size={17} aria-hidden="true" />}</button>
            </span>
            {field.state.meta.errors.length > 0 ? <span className="auth-field-error">{field.state.meta.errors.join(", ")}</span> : null}
          </label>}
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

export function AuthUnavailable({ retry }: { retry: () => void }) {
  return <AuthFrame><div className="auth-heading" role="alert"><p className="eyebrow">Connection needed</p><h2>Sign-in is unavailable</h2><p>The application identity service cannot be reached at this address. Try again or contact your administrator.</p></div><button className="secondary-button" type="button" onClick={retry}><RefreshCw size={16} aria-hidden="true" /> Try again</button></AuthFrame>;
}
