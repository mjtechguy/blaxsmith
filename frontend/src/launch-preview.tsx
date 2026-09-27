import { useState } from "react";
import { ArrowRight, CheckCircle2, FileText, GitBranch, ShieldCheck } from "lucide-react";
import type { PreviewRunResponse } from "./gen/blaxsmith/api/v1/workflow_pb";
import { parseRecipe, stageRows } from "./recipes";

export function LaunchPreview({ preview }: { preview: PreviewRunResponse }) {
  const doc = parseRecipe(preview.recipeJson);
  const rows = stageRows(doc, preview.stageOrder);
  const [selected, setSelected] = useState(rows[0]?.id || "");
  const stage = rows.find((row) => row.id === selected);
  const definition = doc?.stages.find((item) => item.id === selected);
  return <section className="launch-preview" aria-labelledby="launch-preview-heading">
    <div className="table-heading"><div><h2 id="launch-preview-heading"><GitBranch size={18} aria-hidden="true" /> Factory run preview</h2>
      <p>{doc?.factory?.id || "Custom recipe"} · {doc?.name} · commit <code>{preview.sourceCommit.slice(0, 12)}</code></p></div>
      <span className="preview-badge">{preview.canLaunch ? "Ready at preview" : "Needs attention"}</span></div>
    {preview.checkpointId ? <p>Starting checkpoint <code>{preview.checkpointId}</code> · this run requires its own checks and acceptance.</p> : null}
    <p>Choose a stage to inspect its assignment. This preview does not start workers; launch rechecks access and readiness.</p>
    {preview.blockers.length ? <div className="auth-alert" role="status"><strong>Before this run can start</strong><ul>{preview.blockers.map((blocker) => <li key={blocker}>{blocker}</li>)}</ul></div> : null}
    <div className="preview-stages" aria-label="Execution stages">
      {rows.map((row, index) => <button key={row.id} type="button" className="preview-stage" aria-pressed={row.id === selected} aria-controls="preview-stage-detail" onClick={() => setSelected(row.id)}>
        <span className="preview-step">{index + 1}<ArrowRight size={16} aria-hidden="true" /></span><strong>{row.id}</strong><span>{row.kind.replaceAll("_", " ")}</span>
        <small>{row.dependsOn.length ? `After ${row.dependsOn.join(", ")}` : "Start here"}</small>
      </button>)}
      <div className="preview-acceptance"><CheckCircle2 size={22} aria-hidden="true" /><strong>{doc?.acceptance === "policy" ? "Policy acceptance" : "Human acceptance"}</strong><small>After selected requirements pass</small></div>
    </div>
    {stage ? <div className="preview-detail" id="preview-stage-detail" aria-live="polite"><h3>{stage.id}</h3>
      <dl className="launch-prerequisites"><div><dt>Execution</dt><dd>{stage.kind === "verify" ? "Platform checks · no model invocation" : stage.kind === "human_review" ? "Human decision" : `${stage.harness} · ${stage.model} · ${stage.effort || "default effort"}`}</dd></div>
        <div><dt>Inputs</dt><dd>{definition?.prompt ? <code>{definition.prompt}</code> : "Recipe and shared context"}</dd></div>
        {stage.kind === "review" ? <div><dt>Review</dt><dd>{stage.mode || "required"}</dd></div> : null}
        {definition?.review_report ? <div><dt>Review evidence</dt><dd>Required structured artifact <code>{definition.review_report}</code> bound to the candidate and verdict</dd></div> : null}
        {stage.loop ? <div><dt>Correction loop</dt><dd>{stage.loop}</dd></div> : null}
      </dl></div> : null}
    {preview.taskPackets.length ? <section className="preview-packets" aria-label="Frozen task packets"><h3>Task packets · one implementation worker</h3><p>Dependency order is fixed. These assignments do not represent separate workers or completed tasks.</p>
      {preview.taskPackets.map((packet, index) => <details key={packet.taskId} className="observation-provenance"><summary>{index + 1}. {packet.taskId} · {packet.title}</summary><p>Prerequisites: {packet.dependsOn.join(", ") || "None"}</p><p><code>{packet.path}</code> · SHA-256 <code>{packet.sha256}</code></p><pre className="code-block">{JSON.stringify(JSON.parse(packet.contentJson), null, 2)}</pre></details>)}
    </section> : null}
    {preview.repositoryBaselineJson ? <RepositoryBaseline json={preview.repositoryBaselineJson} /> : null}
    {preview.effectiveInputsJson ? <EffectiveInputs json={preview.effectiveInputsJson} /> : null}
    <div className="preview-evidence">
      <div><h3><ShieldCheck size={17} aria-hidden="true" /> Selected checks</h3>
        {preview.checks.length ? <ul>{preview.checks.map((check) => <li key={check.id}><strong>{check.id}</strong> <span className="preview-badge">{check.mode || "required"}</span><code>{JSON.stringify(check.command)}</code></li>)}</ul> : <p>No automated checks selected.</p>}
      </div>
      <div><h3><FileText size={17} aria-hidden="true" /> Frozen inputs · {preview.artifacts.length}</h3>
        <ul>{preview.artifacts.map((artifact) => <li key={artifact.path}><details><summary>{artifact.path}</summary><p>SHA-256 <code>{artifact.sha256}</code></p></details></li>)}</ul>
      </div>
    </div>
  </section>;
}

// The server validates this bounded document before including it in a preview.
type ResolvedInputs = { selected_project_mode?: string; knowledge?: { file: string; sha256: string; title: string; status: string; changed_paths?: string[]; unavailable_paths?: string[] }[]; file: string; sha256: string; commit: string; scope: string; rules?: { id: string; scope: string; file: string; settings?: Record<string, string> }[]; stack?: { name: string; package_manager?: string; prerequisites?: string[]; conventions?: string[]; examples?: string[]; commands?: { id: string; command: string[] }[] }; agent_id?: string; agent?: { responsibility: string; completion_criteria: string[] }; diagnostics?: string[] };
function EffectiveInputs({ json }: { json: string }) {
 let profiles: Record<string, ResolvedInputs>;
 try { profiles = JSON.parse(json); } catch { return <p role="alert">Effective inputs could not be displayed. Preview again before launch.</p>; }
 return <section aria-label="Effective project inputs"><h3>Project rules and agent roles</h3><p>Guidance is pinned to the source commit. Capability requests do not grant access. Stack commands run only when separately selected as project checks.</p>
 {Object.entries(profiles).map(([name, inputs]) => <details key={name} className="observation-provenance"><summary>{name} · {inputs.agent_id || "Project guidance"} · {inputs.rules?.length ?? 0} scoped rules</summary>
 <p>Source <code>{inputs.file}</code> · commit <code>{inputs.commit}</code> · SHA-256 <code>{inputs.sha256}</code></p>
 <p>Selected repository scope: <code>{inputs.scope}</code>{inputs.selected_project_mode ? ` · User-selected mode: ${inputs.selected_project_mode}` : ""}</p>
 {inputs.knowledge?.length ? <><h4>Behavior maps</h4><ul>{inputs.knowledge.map((item) => <li key={item.file}><strong>{item.title}</strong> · {item.status}<p><code>{item.file}</code> · SHA-256 <code>{item.sha256}</code></p>{item.changed_paths?.length ? <p>Changed: {item.changed_paths.join(", ")}</p> : null}{item.unavailable_paths?.length ? <p>Unavailable: {item.unavailable_paths.join(", ")}</p> : null}</li>)}</ul><p>Freshness covers declared file dependencies only. Claims still require review; stale claims are excluded from worker context.</p></> : null}
 {inputs.agent ? <><h4>{inputs.agent_id}</h4><p>{inputs.agent.responsibility}</p><ul>{inputs.agent.completion_criteria.map((item, i) => <li key={i}>{item}</li>)}</ul></> : null}
 {inputs.rules?.length ? <ul>{inputs.rules.map((rule) => <li key={rule.id}><strong>{rule.id}</strong> · directory <code>{rule.scope}</code> · <code>{rule.file}</code>{rule.settings ? <pre className="code-block">{JSON.stringify(rule.settings, null, 2)}</pre> : null}</li>)}</ul> : null}
 {inputs.stack ? <><h4>Stack · {inputs.stack.name}</h4><p>{inputs.stack.package_manager}</p>{inputs.stack.prerequisites?.length ? <p>Prerequisites: {inputs.stack.prerequisites.join("; ")}</p> : null}<ul>{inputs.stack.commands?.map((command) => <li key={command.id}>{command.id}: <code>{JSON.stringify(command.command)}</code> · proposed</li>)}</ul></> : null}
 <ul>{inputs.diagnostics?.map((item, i) => <li key={i}>{item}</li>)}</ul>
 <a className="text-action" download={`${name}-inputs.json`} href={`data:application/json;charset=utf-8,${encodeURIComponent(JSON.stringify({ schema_version: "blaxsmith.inputs/v1alpha1", project_mode: inputs.selected_project_mode, knowledge: inputs.knowledge?.map((item) => item.file), rules: inputs.rules, stack: inputs.stack, agents: inputs.agent_id ? { [inputs.agent_id]: inputs.agent } : undefined }, null, 2))}`}>Export selected configuration</a>
 <details><summary>Exact resolved guidance</summary><pre className="code-block">{JSON.stringify(inputs, null, 2)}</pre></details>
 </details>)}
 </section>;
}

function RepositoryBaseline({ json }: { json: string }) {
 let value: { commit: string; scope: string; detected_mode: string; file_count: number; reasons: string[]; manifests: string[]; instructions: string[]; limitations: string[]; suggested_checks?: { id: string; command: string[] }[] };
 try { value = JSON.parse(json); } catch { return <p role="alert">Repository baseline could not be displayed.</p>; }
 return <section aria-label="Repository baseline"><h3>Repository baseline · {value.detected_mode}</h3><p>{value.file_count} committed files in <code>{value.scope}</code> · <code>{value.commit.slice(0, 12)}</code></p><ul>{value.reasons.map((reason, i) => <li key={i}>{reason}</li>)}</ul>
 <details className="observation-provenance"><summary>Observed setup and limitations</summary><p>Manifests: {value.manifests.join(", ") || "None detected"}</p><p>Instruction files: {value.instructions.join(", ") || "None detected"}</p><ul>{value.suggested_checks?.map((check) => <li key={check.id}><code>{JSON.stringify(check.command)}</code> · suggested, not executed</li>)}</ul><ul>{value.limitations.map((item, i) => <li key={i}>{item}</li>)}</ul></details></section>;
}
