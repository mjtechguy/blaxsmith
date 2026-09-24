import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { PlugZap } from "lucide-react";
import { ConfirmDialog, useOrg } from "../connection-ui";
import { claudePolicyKey, getClaudeSubscriptionPolicy, setClaudeSubscriptionPolicy } from "../connections";
import { StatePanel } from "../ui";

export const Route = createFileRoute("/admin/settings/connections")({ component: ConnectionPolicySettings });

const anthropicTerms = "https://www.anthropic.com/legal/consumer-terms";

// Members' own Claude subscriptions (docs/model-gateway-plan.md §6.1): off by
// default. Enabling asks for confirmation with the provider's terms linked.
function ConnectionPolicySettings() {
  const { org } = useOrg();
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState("");
  const policy = useQuery({ queryKey: claudePolicyKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getClaudeSubscriptionPolicy(signal) });
  const save = useMutation({
    mutationFn: setClaudeSubscriptionPolicy,
    onSuccess: async () => { setConfirming(false); setError(""); await queryClient.invalidateQueries({ queryKey: claudePolicyKey(org) }); },
    onError: (cause) => setError(ConnectError.from(cause).code === Code.PermissionDenied
      ? "Only organization owners and admins can change this setting." : "The setting could not be saved. Please try again."),
  });
  if (policy.isPending) return <StatePanel kind="loading" title="Loading connection settings" />;
  if (policy.isError) return <StatePanel kind="error" title="Connection settings unavailable" retry={() => void policy.refetch()} />;
  const allowed = policy.data;
  return <section className="editor-card" aria-labelledby="conn-policy-heading">
    <div className="editor-card-heading"><span className="project-symbol"><PlugZap size={18} aria-hidden="true" /></span><div>
      <h2 id="conn-policy-heading">Connections</h2>
      <p>Organization rules for what members may connect for themselves.</p></div></div>
    <ul className="flag-list">
      <li className="flag-row"><div>
        <label htmlFor="allow-claude-sub">Allow members to use their own Claude subscription for their own runs</label>
        <p id="allow-claude-sub-help">Each member may paste their own <span className="mono">claude setup-token</span> under My connections. It is used only for runs that member starts: never pooled, shared, or visible to admins, and a run using it can be taken over only by its owner. Turning this off stops existing members' subscriptions at their next run.</p>
      </div>
        <input id="allow-claude-sub" type="checkbox" role="switch" className="switch" checked={allowed} disabled={save.isPending}
          aria-describedby="allow-claude-sub-help" onChange={(e) => { if (e.target.checked) setConfirming(true); else save.mutate(false); }} />
      </li>
    </ul>
    {error && !confirming ? <p className="auth-alert" role="alert">{error}</p> : null}
    {confirming ? <ConfirmDialog tone="primary" title="Allow members' own Claude subscriptions?" confirmLabel="Allow" busy={save.isPending} error={error}
      onClose={() => { setConfirming(false); setError(""); }} onConfirm={() => save.mutate(true)}
      body={<>Members will be able to run their own Blaxsmith work on their personal Claude plan. Review <a className="text-action" href={anthropicTerms} target="_blank" rel="noreferrer">Anthropic's terms</a> for your plan before enabling. This change is audited.</>} /> : null}
  </section>;
}
