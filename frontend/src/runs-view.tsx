// The run collection shared by /runs (every project) and a project's Runs page.
// Server-paged; a row expands to its stages; owners and admins may halt
// selected open runs (AdminService.HaltRun rechecks every target).
import { useState } from "react";
import { ConnectError } from "@connectrpc/connect";
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { OctagonX } from "lucide-react";
import { haltRun } from "./admin";
import { ConfirmDialog } from "./connection-ui";
import { CollectionTable, useUrlView } from "./data-table";
import type { WorkspaceRun } from "./gen/blaxsmith/api/v1/workspace_pb";
import { EmptyState } from "./ui";
import { RunStagesDetail, useRunColumns, useScope } from "./workspace-ui";
import { listWorkspaceRuns, runStates, workspaceRunsKey } from "./workspace";

const defaults = { sort: [{ id: "created", desc: true }], size: 20 };
// Status (the rollup pill) is the primary column; its tooltip carries the raw
// state, and the State column stays available in the Columns menu.
const hiddenByDefault = ["state"];
const openRun = (run: WorkspaceRun) => run.state === "queued" || run.state === "active";

export function RunsCollection({ projectId = "", empty }: { projectId?: string; empty: React.ReactNode }) {
  const { scope, isAdmin } = useScope();
  const queryClient = useQueryClient();
  const [view, setView] = useUrlView(defaults);
  const runs = useQuery({ queryKey: workspaceRunsKey(scope, view, projectId), enabled: Boolean(scope), placeholderData: keepPreviousData, refetchInterval: 20_000,
    queryFn: ({ signal }) => listWorkspaceRuns(view, projectId, signal) });
  const columns = useRunColumns({ showProject: !projectId });
  const [pending, setPending] = useState<{ runs: WorkspaceRun[]; clear: () => void } | null>(null);
  const [outcome, setOutcome] = useState("");
  const halt = useMutation({
    mutationFn: async (targets: WorkspaceRun[]) => {
      const results = await Promise.allSettled(targets.map((run) => haltRun(run.id)));
      return results.map((result, index) => ({ run: targets[index], error: result.status === "rejected" ? ConnectError.from(result.reason).rawMessage || "failed" : "" }));
    },
    onSuccess: async (results) => {
      const failed = results.filter((r) => r.error);
      setOutcome(failed.length ? `${results.length - failed.length} of ${results.length} runs halted. Not halted: ${failed.map((f) => `${f.run.launchKey} (${f.error})`).join(", ")}.`
        : `${results.length} ${results.length === 1 ? "run" : "runs"} halting.`);
      pending?.clear();
      setPending(null);
      await queryClient.invalidateQueries({ queryKey: ["workspace-runs"] });
    },
  });

  return <>
    {outcome ? <p className="notice" role="status">{outcome} <button type="button" className="text-action" onClick={() => setOutcome("")}>Dismiss</button></p> : null}
    <CollectionTable id={projectId ? "project-runs" : "runs"} label={projectId ? "Project runs" : "Runs"} noun="runs" columns={columns} data={runs.data?.runs ?? []} getRowId={(run) => run.id}
      view={view} onView={setView} total={runs.data?.totalCount ?? 0} pinFirst defaultHidden={hiddenByDefault}
      searchLabel={projectId ? "Search run keys or commits" : "Search runs, commits, or projects"} searchNote="Search, filters, and sorting apply to every run on the server."
      facets={[{ id: "state", label: "State", options: runStates }]}
      renderExpanded={(run) => <RunStagesDetail run={run} scope={scope} />} expandLabel={(run) => run.launchKey}
      canSelect={isAdmin ? openRun : undefined}
      bulk={isAdmin ? (selected, clear) => <button type="button" className="secondary-button danger-outline" disabled={halt.isPending || !selected.length}
        onClick={() => { halt.reset(); setPending({ runs: selected, clear }); }}><OctagonX size={14} aria-hidden="true" /> Halt {selected.length} {selected.length === 1 ? "run" : "runs"}</button> : undefined}
      loading={runs.isPending} refreshing={runs.isFetching && !runs.isPending}
      error={runs.isError ? <>Runs could not be loaded. <button type="button" className="text-action" onClick={() => void runs.refetch()}>Try again</button></> : undefined}
      empty={empty} />
    {pending ? <ConfirmDialog title={`Halt ${pending.runs.length} ${pending.runs.length === 1 ? "run" : "runs"}`} confirmLabel="Halt runs" busy={halt.isPending}
      error={halt.isError ? "The runs could not be halted. Please try again." : ""} onClose={() => setPending(null)} onConfirm={() => halt.mutate(pending.runs)}
      body={<>Pending and escalated stages are cancelled, running workers are stopped, and each run ends as cancelled: <strong>{pending.runs.map((r) => r.launchKey).join(", ")}</strong>. This cannot be undone and is recorded in the audit log.</>} /> : null}
  </>;
}

export function NoRuns({ children }: { children?: React.ReactNode }) {
  return <EmptyState title="No runs yet">{children ?? "Runs appear here once a project launches one."}</EmptyState>;
}
