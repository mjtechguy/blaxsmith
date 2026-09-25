import { useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { CircleCheck, CircleSlash, Route as RouteIcon } from "lucide-react";
import { currentSession, sessionQueryKey } from "./auth";
import { accessChain, explainAccess, explainKey } from "./setup";

export function useAccessExplanation(projectId: string, kind: "connection" | "recipe", resourceId: string, principalId = "") {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  return useQuery({ queryKey: explainKey(org, projectId, kind, resourceId, principalId), enabled: Boolean(org && projectId && resourceId),
    queryFn: ({ signal }) => explainAccess(projectId, kind, resourceId, principalId, signal), retry: false, staleTime: 30_000 });
}

// "Where this comes from": the effective chain for the current user or a
// selected principal, or why the resource is not usable here.
export function AccessExplanation({ projectId, kind, resourceId, principalId = "" }: {
  projectId: string; kind: "connection" | "recipe"; resourceId: string; principalId?: string;
}) {
  const explanation = useAccessExplanation(projectId, kind, resourceId, principalId);
  const who = principalId ? explanation.data?.principalLabel || "this user" : "you";
  if (explanation.isPending) return <p className="access-chain" role="status">Checking where this comes from…</p>;
  if (explanation.isError) return <p className="access-chain">Where this comes from could not be checked.</p>;
  const e = explanation.data;
  return <p className={e.usable ? "access-chain" : "access-chain is-blocked"}>
    {e.usable ? <CircleCheck size={14} aria-hidden="true" /> : <CircleSlash size={14} aria-hidden="true" />}
    <span><strong>{e.usable ? `Usable by ${who} via:` : `Not usable by ${who}:`}</strong> {e.usable ? accessChain(e.steps) : e.reason}</span>
  </p>;
}

// Admins check the chain for any project and member from the grants UI.
export function AccessCheck({ kind, resourceId, projectPicker, memberPicker }: {
  kind: "connection" | "recipe"; resourceId: string; projectPicker: (value: string, onChange: (id: string) => void) => ReactNode;
  memberPicker: (value: string, onChange: (id: string) => void) => ReactNode;
}) {
  const [project, setProject] = useState("");
  const [principal, setPrincipal] = useState("");
  const [asked, setAsked] = useState<{ project: string; principal: string } | null>(null);
  return <form className="editor-card editor-form" noValidate aria-label="Check where access comes from"
    onSubmit={(event) => { event.preventDefault(); if (project) setAsked({ project, principal: principal.trim() }); }}>
    <h3 className="access-check-title"><RouteIcon size={15} aria-hidden="true" /> Where this comes from</h3>
    {projectPicker(project, setProject)}
    {memberPicker(principal, setPrincipal)}
    <div className="editor-actions"><button type="submit" className="secondary-button" disabled={!project}>Check access</button></div>
    {asked ? <AccessExplanation projectId={asked.project} kind={kind} resourceId={resourceId} principalId={asked.principal} /> : null}
  </form>;
}
