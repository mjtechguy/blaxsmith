import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowLeft, RefreshCw, Save, Settings } from "lucide-react";
import { failure, LoadError, Loading, useOrg } from "../connection-ui";
import { getGitHubApp, gitHubAppKey, setGitHubApp } from "../connections";
import { TextField } from "../form-field";
import type { GetGitHubAppResponse } from "../gen/blaxsmith/api/v1/connections_pb";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/admin/connections/github-app")({ component: GitHubAppSettings });

function GitHubAppSettings() {
  const { org } = useOrg();
  const app = useQuery({ queryKey: gitHubAppKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getGitHubApp(signal) });
  return <PageShell>
    <PageHeader eyebrow="Administration / Connections" title="GitHub App" description="The GitHub OAuth App that powers Connect GitHub. The client secret is write-only." />
    <Link to="/admin/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    {app.isPending ? <Loading label="Loading GitHub App" /> : null}
    {app.isError ? <LoadError label="GitHub App settings unavailable" retry={() => void app.refetch()} /> : null}
    {app.data ? <AppEditor app={app.data} org={org} /> : null}
  </PageShell>;
}

function AppEditor({ app, org }: { app: GetGitHubAppResponse; org: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const form = useForm({
    defaultValues: { clientId: app.clientId, clientSecret: "" },
    onSubmit: async ({ value }) => {
      setError(""); setSaved(false);
      const clientId = value.clientId.trim();
      const secret = value.clientSecret.trim();
      if (!/^[A-Za-z0-9._-]{1,128}$/.test(clientId) || /[\r\n\0]/.test(secret) || secret.length > 512 || (!app.configured && !secret)) {
        setError("Enter the OAuth App client ID and client secret.");
        return;
      }
      form.setFieldValue("clientSecret", "");
      try {
        await setGitHubApp(clientId, secret);
        await queryClient.invalidateQueries({ queryKey: gitHubAppKey(org) });
        setSaved(true);
      } catch (cause) {
        setError(failure(cause, "The GitHub App settings could not be saved."));
      }
    },
  });
  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="github-app-heading">
      <div className="editor-card-heading"><span className="project-symbol"><Settings size={18} aria-hidden="true" /></span><div><h2 id="github-app-heading">OAuth App registration</h2>
        <p>Status: <strong>{app.configured ? "configured" : "not configured"}</strong>. Callback URL: <span className="mono">{app.callbackUrl || "—"}</span></p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="clientId">{(field) => <TextField label="Client ID" name={field.name} autoComplete="off" placeholder="OAuth App client ID" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}</form.Field>
        <form.Field name="clientSecret">{(field) => <TextField label={app.configured ? "Client secret (blank keeps the stored one)" : "Client secret"} name={field.name} type="password" autoComplete="new-password" placeholder="Paste the client secret" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} required={!app.configured} />}</form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        {saved ? <p className="notice" role="status">Saved.</p> : null}
        <div className="editor-actions"><form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Save size={15} aria-hidden="true" />}{submitting ? "Saving…" : "Save"}</button>}
        </form.Subscribe></div>
      </form>
    </section>
    <aside className="editor-note"><h2>Setup</h2><p>Create a GitHub OAuth App with the callback URL above, then paste its client ID and secret here. People then connect with GitHub sign-in instead of pasting tokens; the token form stays available under Advanced.</p></aside>
  </div>;
}
