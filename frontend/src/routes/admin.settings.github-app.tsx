import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { Settings } from "lucide-react";
import { failure, useOrg } from "../connection-ui";
import { getGitHubApp, gitHubAppKey, setGitHubApp } from "../connections";
import { TextField } from "../form-field";
import type { GetGitHubAppResponse } from "../gen/blaxsmith/api/v1/connections_pb";
import { GuardedSaveBar, useSaved } from "../layouts";
import { CopyValue, StatePanel } from "../ui";

export const Route = createFileRoute("/admin/settings/github-app")({ component: GitHubAppSettings });

function GitHubAppSettings() {
  const { org } = useOrg();
  const app = useQuery({ queryKey: gitHubAppKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getGitHubApp(signal) });
  if (app.isPending) return <StatePanel kind="loading" title="Loading GitHub app" />;
  if (app.isError) return <StatePanel kind="error" title="GitHub app settings unavailable" retry={() => void app.refetch()} />;
  return <AppEditor app={app.data} org={org} />;
}

function AppEditor({ app, org }: { app: GetGitHubAppResponse; org: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [saved, setSaved] = useSaved();
  const form = useForm({
    defaultValues: { clientId: app.clientId, clientSecret: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const clientId = value.clientId.trim();
      const secret = value.clientSecret.trim();
      if (!/^[A-Za-z0-9._-]{1,128}$/.test(clientId) || /[\r\n\0]/.test(secret) || secret.length > 512 || (!app.configured && !secret)) {
        setError("Enter the OAuth App client ID and client secret.");
        return;
      }
      try {
        await setGitHubApp(clientId, secret);
        await queryClient.invalidateQueries({ queryKey: gitHubAppKey(org) });
        form.reset({ clientId, clientSecret: "" });
        setSaved(true);
      } catch (cause) {
        setError(failure(cause, "The GitHub app settings could not be saved."));
      }
    },
  });
  return <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="github-app-heading">
      <div className="editor-card-heading"><span className="project-symbol"><Settings size={18} aria-hidden="true" /></span><div><h2 id="github-app-heading">GitHub OAuth app</h2>
        <p>{app.configured ? "Configured. People connect with GitHub sign-in instead of pasting tokens." : "Not configured. Until it is, Git access uses pasted tokens."} The client secret is write-only.</p></div></div>
      <div className="editor-form">
        <div className="form-field"><span>Callback URL</span>{app.callbackUrl ? <CopyValue value={app.callbackUrl} label="Callback URL" chars={48} /> : <span className="muted">—</span>}
          <small className="form-hint">Create a GitHub OAuth App with this callback URL, then paste its client ID and secret.</small></div>
        <form.Field name="clientId">{(field) => <TextField label="Client ID" name={field.name} autoComplete="off" placeholder="OAuth App client ID" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}</form.Field>
        <form.Field name="clientSecret">{(field) => <TextField label={app.configured ? "Client secret (blank keeps the stored one)" : "Client secret"} name={field.name} type="password" autoComplete="new-password" placeholder="Paste the client secret" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} required={!app.configured} />}</form.Field>
      </div>
    </section>
    <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
      {([dirty, submitting, canSubmit]) => <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved} onCancel={() => { form.reset(); setError(""); }} />}
    </form.Subscribe>
  </form>;
}

