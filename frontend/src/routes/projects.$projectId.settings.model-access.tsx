import { useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, KeyRound } from "lucide-react";
import { deliveryLabels, getProjectDelivery, projectDeliveryKey, setProjectDelivery } from "../gateway";
import type { GetProjectDeliveryResponse } from "../gen/blaxsmith/api/v1/gateway_pb";
import { GuardedSaveBar, useSaved } from "../layouts";
import { StatePanel } from "../ui";
import { useScope } from "../workspace-ui";

export const Route = createFileRoute("/projects/$projectId/settings/model-access")({ component: ModelAccess });

// Project → Settings → Model access: how this project's runs receive model
// credentials (docs/model-gateway-plan.md §3, §15.1).
function ModelAccess() {
  const { projectId } = Route.useParams();
  const { org } = useScope();
  const delivery = useQuery({ queryKey: projectDeliveryKey(org, projectId), enabled: Boolean(org), queryFn: ({ signal }) => getProjectDelivery(projectId, signal) });
  if (delivery.isPending) return <StatePanel kind="loading" title="Loading model access" />;
  if (delivery.isError) return <StatePanel kind="error" title="Model access settings unavailable" retry={() => void delivery.refetch()} />;
  return <Editor key={`${delivery.data.projectChoice}:${delivery.data.choiceAllowed}`} data={delivery.data} org={org} projectId={projectId} />;
}

function Editor({ data, org, projectId }: { data: GetProjectDeliveryResponse; org: string; projectId: string }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const [saved, setSaved] = useSaved();
  const editable = data.canEdit && data.choiceAllowed && data.gatewayEnabled;
  const form = useForm({
    defaultValues: { mode: data.projectChoice },
    onSubmit: async ({ value }) => {
      setError("");
      try {
        await setProjectDelivery(projectId, value.mode);
        await queryClient.invalidateQueries({ queryKey: projectDeliveryKey(org, projectId) });
        setSaved(true);
      } catch (cause) {
        setError(ConnectError.from(cause).code === Code.PermissionDenied ? "Only project admins can change this, and only while the organization lets projects choose." : "The setting could not be saved. Please try again.");
      }
    },
  });
  const note = !data.gatewayEnabled ? "The organization has not turned on the model gateway, so runs receive provider keys directly."
    : !data.choiceAllowed ? `The organization enforces its default: ${deliveryLabels[data.orgDefault] ?? data.orgDefault}.`
      : !data.canEdit ? "Only project admins can change how this project's runs receive model access." : "";
  return <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="model-access-heading">
      <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="model-access-heading">Model access</h2>
        <p>Next runs use <strong>{deliveryLabels[data.deliveryMode] ?? data.deliveryMode}</strong>. With the model gateway, sandboxes get a short-lived run token and Blaxsmith injects the key, so no provider key reaches the agent or a person who takes over its terminal. Direct keys are delivered into the sandbox for the length of the stage.</p></div></div>
      <div className="editor-form">
        <form.Field name="mode">{(field) => <label className="form-field"><span>Delivery mode</span>
          <select value={field.state.value} disabled={!editable} onChange={(e) => field.handleChange(e.target.value)} aria-describedby="model-access-note">
            <option value="">Organization default: {data.orgDefault === "brokered_gateway" ? "model gateway" : "direct key"}</option>
            <option value="brokered_gateway">{deliveryLabels.brokered_gateway}</option>
            <option value="native_raw">{deliveryLabels.native_raw}</option>
          </select>
          <small className="form-hint" id="model-access-note">{note || "Applies to stages dispatched after you save; running stages keep the mode they started with."}</small></label>}</form.Field>
        <Link className="text-action" to="/projects/$projectId/connections" params={{ projectId }}>Connections and model grants <ArrowRight size={13} aria-hidden="true" /></Link>
      </div>
    </section>
    {editable ? <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting] as const}>
      {([dirty, submitting]) => <GuardedSaveBar dirty={dirty} saving={submitting} error={error} saved={saved} onCancel={() => { form.reset(); setError(""); }} />}
    </form.Subscribe> : null}
  </form>;
}
