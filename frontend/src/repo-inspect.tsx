import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, FileSearch } from "lucide-react";
import { currentSession, sessionQueryKey } from "./auth";
import type { InspectRepositoryResponse } from "./gen/blaxsmith/api/v1/setup_pb";
import { inspectKey, inspectRepository } from "./setup";

// What the project's repository suggests at its pinned commit: a committed
// .blaxsmith.json, else detected manifests (docs/project-file.md). The fetch
// is slow, so the result is cached for a few minutes and never retried.
export function useRepositoryInspection(projectId: string, enabled = true) {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  return useQuery({ queryKey: inspectKey(org, projectId), enabled: Boolean(org && projectId) && enabled,
    queryFn: ({ signal }) => inspectRepository(projectId, signal), staleTime: 5 * 60_000, retry: false });
}

export const suggestionSource = (r: Pick<InspectRepositoryResponse, "source" | "evidence">) =>
  r.source === "file" ? ".blaxsmith.json" : r.evidence.join(", ");

// A compact summary with an optional link to review the suggested checks.
export function RepositorySuggestion({ projectId, reviewLink = false }: { projectId: string; reviewLink?: boolean }) {
  const inspection = useRepositoryInspection(projectId);
  if (inspection.isPending) return <p className="access-chain" role="status"><FileSearch size={14} aria-hidden="true" /> Reading the repository for suggestions…</p>;
  if (inspection.isError) return <p className="access-chain">The repository could not be read for suggestions.</p>;
  const r = inspection.data;
  return <div className="repo-suggestion">
    <p className="access-chain"><FileSearch size={14} aria-hidden="true" /><span>
      {r.source === "none" ? "No checks detected in this repository." : <><strong>Suggested from {suggestionSource(r)}</strong> at <code>{r.commit.slice(0, 12)}</code>: {r.verification.length} {r.verification.length === 1 ? "check" : "checks"}{r.recipe ? `, recipe “${r.recipe}”` : ""}.</>}
    </span></p>
    {r.fileError ? <p className="form-field-error">.blaxsmith.json was ignored: {r.fileError}</p> : null}
    {r.setup.length ? <p className="form-hint">Setup (not run by Blaxsmith yet): {r.setup.map((c) => <code key={c.id}>{c.command.join(" ")}</code>)}</p> : null}
    {reviewLink && r.verification.length ? <Link className="text-action" to="/projects/$projectId/verification" params={{ projectId }}>Review suggested checks <ArrowRight size={13} aria-hidden="true" /></Link> : null}
  </div>;
}
