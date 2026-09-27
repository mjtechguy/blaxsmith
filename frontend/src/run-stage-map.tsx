import { Link } from "@tanstack/react-router";
import { ArrowRight, Bot, GitBranch, ListChecks, Repeat, ShieldCheck } from "lucide-react";
import type { ReviewPackage, RunTask } from "./gen/blaxsmith/api/v1/workflow_pb";
import { stageLayers, type AgentStatus } from "./agent-view";
import { StatusPill } from "./work-log";
import { sentence } from "./ui";

export function RunStageMap({ tasks, statuses, projectId, runId, selected, state, review, reviewError }: {
  tasks: RunTask[]; statuses: Map<string, AgentStatus | null>; projectId: string; runId: string;
  selected?: string; state: string; review?: ReviewPackage | null; reviewError: boolean;
}) {
  const { layers, unresolved } = stageLayers(tasks);
  const params = { projectId, runId };
  const destination = "/projects/$projectId/runs/$runId";
  const acceptance = reviewError ? "Acceptance unavailable" : state !== "succeeded" ? "After execution and checks"
    : !review ? "Awaiting evidence package" : review.decision?.action === "request_changes" ? "Changes requested"
    : review.acceptanceMode === "policy" ? "Accepted by policy" : review.decision?.action === "approve" ? "Approved by a person" : "Awaiting human decision";
  const node = (task: RunTask) => <li key={task.id} className="factory-node">
    <Link to={destination} params={params} search={{ tab: "stages", stage: task.key }} className="factory-stage" aria-current={selected === task.key ? "step" : undefined} aria-label={`Open stage ${task.key}`}>
      <span className="factory-node-title">{task.kind === "verify" ? <ListChecks size={18} aria-hidden="true" /> : <Bot size={18} aria-hidden="true" />}<strong>{task.key}</strong></span>
      <span>{task.kind === "verify" ? "Platform checks · no model" : task.kind === "human_review" ? "Human decision" : [task.harness, task.model].filter(Boolean).join(" · ")}</span>
      <span>{statuses.get(task.key) ? <StatusPill status={statuses.get(task.key)!} /> : null} <small>{sentence(task.state)}</small></span>
      <small>{task.generation.toString()} attempt{task.generation === 1n ? "" : "s"} reserved</small>
    </Link>
    <div className="factory-node-context">
      <span>{task.dependsOn.length ? "After " : "Start here"}{task.dependsOn.map((key, i) => <span key={key}>{i ? ", " : ""}<Link to={destination} params={params} search={{ tab: "stages", stage: key }} className="text-action">{key}</Link></span>)}</span>
      {task.loopWith ? <span className="factory-loop"><Repeat size={14} aria-hidden="true" /><span>{task.loopCycles} / {task.maxCycles} repairs requested · back to <Link to={destination} params={params} search={{ tab: "stages", stage: task.loopWith }} className="text-action">{task.loopWith}</Link></span></span> : null}
    </div>
  </li>;
  return <section className="table-section" aria-labelledby="factory-map-heading">
    <div className="table-heading"><div><h2 id="factory-map-heading"><GitBranch size={18} aria-hidden="true" /> Factory map</h2><p>Open a stage for its work log, questions, and controls. Stages in the same column have independent dependencies; they may run when resources allow.</p></div></div>
    {!tasks.length ? <p className="card-body">No stages have been frozen for this run.</p> : <div className="factory-map" role="region" aria-label="Stage dependencies and acceptance" tabIndex={0}>
      {layers.map((layer, i) => <div className="factory-layer" key={i}><h3>Step {i + 1}<ArrowRight size={16} aria-hidden="true" /></h3><ul>{layer.map(node)}</ul></div>)}
      {unresolved.length ? <div className="factory-layer"><h3>Dependencies unavailable</h3><ul>{unresolved.map(node)}</ul></div> : null}
      <div className="factory-layer factory-finish"><h3>Acceptance</h3><Link to={destination} params={params} search={{ tab: "review" }} className="factory-stage">
        <ShieldCheck size={20} aria-hidden="true" /><strong>{acceptance}</strong><span>Inspect checks, artifacts, and the candidate</span>
        {state === "succeeded" && review?.integratedCommit ? <small>Candidate {review.integratedCommit.slice(0, 12)}</small> : null}
      </Link><p>Successful execution and acceptance do not mean the target branch was merged.</p></div>
    </div>}
  </section>;
}
