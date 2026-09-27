import { useState } from "react";
import { useQuery, useMutation, useInfiniteQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { csrfToken } from "../auth";
import { PageHeader, PageShell } from "../page";
import { useScope } from "../workspace-ui";
import { listProjects } from "../workflow";

export const Route = createFileRoute("/oauth/consent")({
 validateSearch: (search: Record<string, unknown>) => ({ request: typeof search.request === "string" ? search.request : "" }),
 component: OAuthConsent,
});
type RequestDetails = { client_id: string; client_name: string; redirect_uri: string; resource: string; scopes: string[]; deny_url: string };
const scopeLabels: Record<string, string> = {
 "project.read": "Read project goals, plans, runs, artifacts and evidence",
 "goal.write": "Create and edit goals, plans and accepted checkpoints",
 "run.launch": "Launch work using your available model grants (may incur costs)",
 "run.control": "Answer questions, steer work and control goals",
 "review.decide": "Approve or request changes on review packages",
};
async function oauthRequest<T>(path: string, body: object, signal?: AbortSignal): Promise<T> {
 const response = await fetch(path, { method: "POST", credentials: "same-origin", signal,
  headers: { "Content-Type": "application/json", "X-Blaxsmith-CSRF": await csrfToken() }, body: JSON.stringify(body) });
 if (!response.ok) throw new Error("Authorization could not complete. Check your session and the client's request, then try again.");
 return response.json() as Promise<T>;
}
function OAuthConsent() {
 const { request } = Route.useSearch(); const { scope } = useScope();
 const details = useQuery({ queryKey: ["oauth-consent", scope, request], enabled: !!scope && !!request,
  queryFn: ({ signal }) => oauthRequest<RequestDetails>("/oauth/request", { request }, signal), retry: false });
 return <PageShell><PageHeader title="Authorize an MCP client" description="Choose the project and permissions this client can use on your behalf." />
  {!request || details.isError ? <p role="alert">This authorization request is invalid or unavailable. Return to the client and start again.</p> : null}
  {request && details.isPending ? <p role="status">Checking the authorization request…</p> : null}
  {details.data ? <ConsentForm key={`${scope}:${request}`} request={request} details={details.data} /> : null}
 </PageShell>;
}
function ConsentForm({ request, details }: { request: string; details: RequestDetails }) {
 const { scope, role } = useScope();
 const projects = useInfiniteQuery({ queryKey: ["oauth-projects", scope], initialPageParam: "", queryFn: ({ pageParam, signal }) => listProjects(pageParam, "", "name", "asc", signal), getNextPageParam: (last) => last.nextPageToken || undefined });
 const [projectId, setProject] = useState("");
 const [scopes, setScopes] = useState<string[]>(details.scopes.filter((s) => s === "project.read"));
 const approve = useMutation({ mutationFn: () => oauthRequest<{ redirect_url: string }>("/oauth/approve", { request, project_id: projectId, scopes }),
  onSuccess: (result) => { window.location.assign(result.redirect_url); } });
 return <section className="editor-card oauth-consent"><header className="editor-card-heading"><div><h2>{details.client_name}</h2>
  <p>This client will act as you within one project. Access expires after one hour and can be revoked in that project's Agent access settings.</p></div></header>
  <dl className="review-facts"><div><dt>Client ID</dt><dd><code>{details.client_id}</code></dd></div><div><dt>Return address</dt><dd><code>{details.redirect_uri}</code></dd></div><div><dt>Protected resource</dt><dd><code>{details.resource}</code></dd></div></dl>
  <form className="editor-form" onSubmit={(event) => { event.preventDefault(); if (projectId) approve.mutate(); }}><fieldset disabled={approve.isPending}><legend className="sr-only">Client permissions</legend>
   <label className="form-field"><span>Project to authorize</span><select required value={projectId} onChange={(event) => setProject(event.target.value)}><option value="">Choose a project</option>{projects.data?.pages.flatMap((page) => page.projects).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>
   {projects.isPending ? <p role="status">Loading projects…</p> : null}
   {projects.hasNextPage ? <button className="text-action" type="button" disabled={projects.isFetchingNextPage} onClick={() => void projects.fetchNextPage()}>Load more projects</button> : null}
   {projects.isError ? <p role="alert">Projects could not load. <button className="text-action" type="button" onClick={() => void projects.refetch()}>Retry</button></p> : null}
   <p>Only read access is selected by default. Select any additional permissions you want to grant.</p>
   <fieldset className="agent-permissions"><legend>Requested permissions</legend>{["project.read", ...details.scopes.filter((s) => s !== "project.read")].map((s) => <label key={s}><input type="checkbox" checked={scopes.includes(s)} disabled={s === "project.read" || role === "viewer"} onChange={(event) => setScopes((old) => event.target.checked ? [...old, s] : old.filter((item) => item !== s))} /><span>{scopeLabels[s] ?? s}</span></label>)}</fieldset>
   <div className="editor-actions"><button className="secondary-button" type="button" onClick={() => window.location.assign(details.deny_url)}>Deny access</button><button className="primary-button" disabled={!projectId || projects.isError}>{approve.isPending ? "Authorizing…" : "Authorize client"}</button></div>
  </fieldset></form>
  {approve.isError ? <p role="alert">{approve.error.message}</p> : null}
 </section>;
}
