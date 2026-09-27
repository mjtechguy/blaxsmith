import { useRef, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { ConnectError, createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "../auth";
import { WorkflowService } from "../gen/blaxsmith/api/v1/workflow_pb";
import { useScope } from "../workspace-ui";
import { Card, StatePanel } from "../ui";

export const Route = createFileRoute("/projects/$projectId/settings/agents")({ component: AgentAccess });
const client = createClient(WorkflowService, browserTransport);
const permissions = [
 ["project.read", "Read project, goals, runs and evidence"],
 ["goal.write", "Create goals, answer starter questions and save plans"],
 ["run.launch", "Preview and launch work using authorized models (may incur costs)"],
 ["run.control", "Answer run questions and request pause, resume or halt"],
 ["review.decide", "Accept current review packages or request corrections"],
] as const;

function AgentAccess() {
 const { projectId } = Route.useParams(); const { scope, role } = useScope();
 const admin = role === "owner" || role === "admin";
 const [service, setService] = useState(""); const [serviceLabel, setServiceLabel] = useState(""); const serviceKey = useRef<{ label: string; key: string } | undefined>(undefined);
 const services = useQuery({ queryKey: ["service-principals", scope, projectId], queryFn: ({ signal }) => client.listServicePrincipals({ projectId }, { signal }), enabled: !!scope && admin });
 const [label, setLabel] = useState(""); const [scopes, setScopes] = useState<string[]>(["project.read"]); const [lifetime, setLifetime] = useState(86400);
 const [busy, setBusy] = useState(false); const [error, setError] = useState(""); const [issued, setIssued] = useState<{ id: string; token: string }>(); const [copied, setCopied] = useState(false);
 const query = useQuery({ queryKey: ["api-tokens", scope, projectId, service], queryFn: ({ signal }) => client.listApiTokens({ projectId, servicePrincipalId: service }, { signal }), enabled: !!scope });
 const issue = async () => {
  setBusy(true); setError(""); setCopied(false);
  try { const result = await client.createApiToken({ projectId, servicePrincipalId: service, label, scopes, lifetimeSeconds: lifetime }, { headers: { "X-Blaxsmith-CSRF": await csrfToken() } }); setIssued({ id: result.credential!.id, token: result.token }); setLabel(""); await query.refetch(); }
  catch (cause) { setError(`${ConnectError.from(cause).rawMessage}. If the response was lost, inspect and revoke the new credential before creating another.`); }
  finally { setBusy(false); }
 };
 const revoke = async (id: string) => {
  setBusy(true); setError("");
  try { await client.revokeApiToken({ tokenId: id }, { headers: { "X-Blaxsmith-CSRF": await csrfToken() } }); if (issued?.id === id) setIssued(undefined); await query.refetch(); }
  catch (cause) { setError(ConnectError.from(cause).rawMessage); } finally { setBusy(false); }
 };
 const createService = async () => {
  setBusy(true); setError("");
  if (serviceKey.current?.label !== serviceLabel) serviceKey.current = { label: serviceLabel, key: crypto.randomUUID() };
  try { const result = await client.createServicePrincipal({ projectId, label: serviceLabel, requestKey: serviceKey.current.key }, { headers: { "X-Blaxsmith-CSRF": await csrfToken() } }); serviceKey.current = undefined; setService(result.principal!.id); setScopes(["project.read"]); setServiceLabel(""); await services.refetch(); }
  catch (cause) { setError(ConnectError.from(cause).rawMessage); } finally { setBusy(false); }
 };
 const disableService = async () => {
  setBusy(true); setError("");
  try { await client.disableServicePrincipal({ principalId: service }, { headers: { "X-Blaxsmith-CSRF": await csrfToken() } }); setIssued(undefined); await Promise.all([services.refetch(), query.refetch()]); }
  catch (cause) { setError(ConnectError.from(cause).rawMessage); } finally { setBusy(false); }
 };
 const serviceDisabled = !!service && services.data?.principals.find((p) => p.id === service)?.state !== "active";
 return <Card title="Agent access" description="Project-scoped credentials for external factories, scripts and the Blaxsmith MCP bridge. Choose delegated user access or an administrator-managed service identity.">
  <div className="card-body editor-form">
   <p>Credentials expire independently of browser sign-out. A membership or role change invalidates access. Revoking a credential stops future requests; it does not cancel existing runs.</p>
   {admin ? <section className="editor-form" aria-label="Service identities"><h3>Credential identity</h3>
    <label className="form-field"><span>Act as</span><select value={service} disabled={busy} onChange={(e) => { setService(e.target.value); setIssued(undefined); setScopes(["project.read"]); }}><option value="">You · delegated user</option>{services.data?.principals.map((p) => <option key={p.id} value={p.id}>{p.label} · service · {p.state}</option>)}</select></label>
    <p>Service identities belong to this project, have no human login, and use project model grants. They cannot mint credentials, change allowances or make human review decisions. Administrators can issue replacement credentials without changing the service’s goal ownership.</p>
    {services.isError ? <StatePanel kind="error" title="Service identities unavailable" retry={() => void services.refetch()} /> : null}
    {service ? <details><summary>Disable service identity…</summary><p>Disables this identity and revokes all its credentials. Existing runs require their own cancellation.</p><button type="button" className="secondary-button" disabled={busy || serviceDisabled} onClick={() => void disableService()}>Disable selected service identity</button></details> : null}
    <details><summary>Add service identity</summary><form className="editor-form" onSubmit={(e) => { e.preventDefault(); void createService(); }}><label className="form-field"><span>Service identity name</span><input required maxLength={100} value={serviceLabel} onChange={(e) => setServiceLabel(e.target.value)} /></label><button className="secondary-button" disabled={busy || !serviceLabel.trim()}>Create service identity</button></form></details>
   </section> : null}
   {error ? <p role="alert" className="auth-alert">{error}</p> : null}
   {issued ? <section className="editor-form" aria-label="New credential"><p>Copy this token now. It is shown once and is not recoverable after leaving this page.</p><label className="form-field"><span>API token</span><input type="password" readOnly autoComplete="off" value={issued.token} onFocus={(e) => e.currentTarget.select()} /></label>
    <div className="editor-actions"><button type="button" className="secondary-button" onClick={async () => { try { await navigator.clipboard.writeText(issued.token); setCopied(true); } catch { setError("Copy failed. Select the token and copy it manually."); } }}>Copy token</button><button type="button" className="text-action" onClick={() => setIssued(undefined)}>Dismiss token</button></div>{copied ? <p role="status">Token copied.</p> : null}
   </section> : <form className="editor-form" onSubmit={(e) => { e.preventDefault(); void issue(); }}><fieldset disabled={busy || serviceDisabled}><legend className="sr-only">New API credential</legend>
    <label className="form-field"><span>Credential label</span><input required maxLength={100} value={label} onChange={(e) => setLabel(e.target.value)} /></label>
    <label className="form-field"><span>Expires after</span><select value={lifetime} onChange={(e) => setLifetime(Number(e.target.value))}><option value={3600}>1 hour</option><option value={86400}>1 day</option><option value={604800}>7 days</option><option value={2592000}>30 days</option></select></label>
    <fieldset className="agent-permissions"><legend>Allowed operations</legend>{permissions.filter(([id]) => (role !== "viewer" || id === "project.read") && (!service || id !== "review.decide")).map(([id, text]) => <label key={id}><input type="checkbox" checked={scopes.includes(id)} onChange={(e) => setScopes(e.target.checked ? [...scopes, id] : scopes.filter((s) => s !== id))} /><span>{text}</span></label>)}</fieldset>
    <button className="primary-button" disabled={!label.trim() || !scopes.length}>Create credential</button>
   </fieldset></form>}
   {query.isError ? <StatePanel kind="error" title="Credentials unavailable" retry={() => void query.refetch()} /> : null}
   <h3>{service ? "Service credentials for this project" : "Your credentials for this project"}</h3>{query.isPending ? <p role="status">Loading credentials…</p> : null}
   <ul className="agent-credentials">{query.data?.credentials.map((item) => <li key={item.id}><div><strong>{item.label}</strong><p>{item.scopes.join(", ")}</p>{item.resource ? <p>MCP only: <code>{item.resource}</code></p> : null}<p>{item.revokedAt ? "Revoked" : `Expires ${new Date(item.expiresAt).toLocaleString()}`}</p></div><button type="button" className="secondary-button" disabled={busy || !!item.revokedAt} aria-label={`Revoke ${item.label}`} onClick={() => void revoke(item.id)}>Revoke</button></li>)}</ul>
   <details className="observation-provenance"><summary>Connect MCP or an API client</summary><p>Run <code>blaxsmith mcp --server {typeof window === "undefined" ? "https://your-blaxsmith.example" : window.location.origin}</code> with <code>BLAXSMITH_API_TOKEN</code> set in the MCP host's environment. Keep the token out of prompts and source files.</p><p>Direct clients use the generated WorkflowService at <code>/machine/blaxsmith.api.v1.WorkflowService/</code> with a Bearer Authorization header. Browser cookies are not accepted there.</p></details>
  </div>
 </Card>;
}
