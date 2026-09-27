import type { AdvicePhase } from "./model-advice";
import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { listProjectRecipeFiles, recipeFilesKey } from "./recipes";
import { ModelSelect } from "./model-select";
import { getProjectModelOptions, type PlanningInput } from "./goals";
export type GoalRunSettingsValue = Pick<PlanningInput, "connectionId" | "harness" | "model" | "effort" | "scope" | "runtimeSeconds" | "instructionFiles" | "skillFiles" | "inputsFile" | "agentDefinition">;
export const defaultGoalRunSettings: GoalRunSettingsValue = { harness: "codex", model: "", effort: "medium", scope: ".", runtimeSeconds: 900, instructionFiles: [], skillFiles: [], inputsFile: "", agentDefinition: "" };

export function GoalRunSettings({ scope, projectId, value, onChange, profileOnly = false, phase = "planning" }: { scope: string; projectId: string; value: GoalRunSettingsValue; onChange: (value: GoalRunSettingsValue) => void; profileOnly?: boolean; phase?: AdvicePhase }) {
 const fieldId = useId();
 const options = useQuery({ queryKey: ["goal-model-options", scope, projectId, value.harness], queryFn: ({ signal }) => getProjectModelOptions(projectId, value.harness, signal), staleTime: 30_000 });
 const eligible = options.data?.connections ?? [];
 const selectedModel = value.harness === "opencode" && value.model.includes("/") ? value.model.slice(value.model.indexOf("/") + 1) : value.model;
 const selectModel = (model: string, id: string | undefined, effort?: string) => {
  const provider = eligible.find((c) => c.connectionId === id)?.provider;
  onChange({ ...value, connectionId: id || "", model: value.harness === "opencode" && provider && model && !model.includes("/") ? `${provider}/${model}` : model, effort: effort || value.effort });
 };
 return <><div className="recipe-grid">
  <label className="form-field"><span>Harness</span><select value={value.harness} onChange={(e) => onChange({ ...value, harness: e.target.value, model: "", connectionId: "" })}>{["codex", "claude-code", "opencode"].map((h) => <option key={h}>{h}</option>)}</select></label>
  <label className="form-field"><span>Effort</span><input value={value.effort} onChange={(e) => onChange({ ...value, effort: e.target.value })} required maxLength={32} /></label></div>
  <ModelSelect connectionId={value.connectionId || ""} connections={eligible.map((c) => ({ id: c.connectionId, label: `${c.label || c.provider} · ${c.ownerKind === "user" ? "Personal" : c.ownerKind} · ${c.authMethod}`, provider: c.provider, models: c.models, checkedAt: c.checkedAt, error: c.catalogError }))} advicePhase={phase} onAdviceEffort={(model, id, _info, effort) => selectModel(model, id, effort)} harness={value.harness} fieldId={fieldId} value={selectedModel} onChange={(model, id, info) => selectModel(model, id, info?.defaultEffort)} />
  <label className="form-field"><span>Model account (connection ID)</span><input value={value.connectionId ?? ""} maxLength={64} onChange={(e) => onChange({ ...value, connectionId: e.target.value })} placeholder="Select a listed model to pin its account" /></label>
  <p className="form-hint">A selected account is frozen for this phase and never falls back to another account. An empty account uses your active personal grant, otherwise the project’s model grant. Runtime approval and current access are checked again at launch.</p>
  {!profileOnly ? <div className="recipe-grid"><label className="form-field"><span>Repository scope</span><input value={value.scope} onChange={(e) => onChange({ ...value, scope: e.target.value })} required /></label><label className="form-field"><span>Seconds per stage</span><input type="number" min={60} max={3600} value={value.runtimeSeconds} onChange={(e) => onChange({ ...value, runtimeSeconds: Number(e.target.value) })} required /></label></div> : null}
  <GoalContextFiles projectId={projectId} scope={scope} value={value} onChange={onChange} />
  {options.isError ? <p role="alert">Model connections could not be loaded. You can enter an exact model ID under Advanced.</p> : null}
 </>;
}

function GoalContextFiles({ projectId, scope, value, onChange }: { projectId: string; scope: string; value: GoalRunSettingsValue; onChange: (value: GoalRunSettingsValue) => void }) {
 const [expanded, setExpanded] = useState(false);
 const files = useQuery({ queryKey: recipeFilesKey(scope, projectId), queryFn: ({ signal }) => listProjectRecipeFiles(projectId, signal), enabled: expanded, staleTime: 60_000, retry: false });
 return <details className="observation-provenance" onToggle={(e) => setExpanded(e.currentTarget.open)}><summary>Repository rules and skills · {(value.instructionFiles?.length ?? 0) + (value.skillFiles?.length ?? 0)} selected</summary>
  <p className="form-hint">Applicable AGENTS.md files are included automatically. Add committed rules for your stack, architecture, testing, or agent role. Selections apply to this phase only.</p>
  <div className="recipe-grid"><label className="form-field"><span>Project inputs file</span><input value={value.inputsFile ?? ""} maxLength={1024} placeholder=".blaxsmith/inputs.json" onChange={(e) => onChange({ ...value, inputsFile: e.target.value, agentDefinition: e.target.value ? value.agentDefinition : "" })} /></label>
  <label className="form-field"><span>Agent definition</span><input value={value.agentDefinition ?? ""} disabled={!value.inputsFile} maxLength={64} placeholder="implementer" onChange={(e) => onChange({ ...value, agentDefinition: e.target.value })} /></label></div>
  <p className="form-hint">Optional committed configuration supplies scoped rules, stack conventions and an agent role. Your model selection remains explicit. Preview shows resolved inputs and reports unsupported capabilities or conflicting rules.</p>
  <ContextPaths label="Instruction files" paths={value.instructionFiles ?? []} choices={files.data?.promptPaths ?? []} placeholder="docs/engineering-rules.md" onChange={(instructionFiles) => onChange({ ...value, instructionFiles })} />
  <ContextPaths label="Repository skills" paths={value.skillFiles ?? []} choices={files.data?.skillPaths ?? []} placeholder="skills/testing/SKILL.md" onChange={(skillFiles) => onChange({ ...value, skillFiles })} />
  <p className="form-hint">Use repository-relative paths. Select each SKILL.md and any supporting files it needs under the same directory; only selected files are installed. Skills do not grant tools or bypass project checks. Preview and launch pin the actual file bytes. Planning and implementation selections are independent.</p>
  {files.isFetching ? <p role="status">Looking up repository files…</p> : null}
  {files.data ? <p className="form-hint">Suggestions from commit <code>{files.data.commit.slice(0, 12)}</code>. You can also enter an exact path. <button type="button" className="text-action" disabled={files.isFetching} onClick={() => void files.refetch()}>Refresh files</button></p> : null}
  {files.isError ? <p role="alert">Could not list repository files. Enter exact paths or <button type="button" className="text-action" onClick={() => void files.refetch()}>try again</button>. Paths are checked before launch.</p> : null}
 </details>;
}

function ContextPaths({ label, paths, choices, placeholder, onChange }: { label: string; paths: string[]; choices: string[]; placeholder: string; onChange: (paths: string[]) => void }) {
 const id = useId(); const [draft, setDraft] = useState("");
 const add = () => { const path = draft.trim(); if (path && !paths.includes(path)) onChange([...paths, path]); setDraft(""); };
 return <fieldset className="recipe-fieldset"><legend>{label}</legend>
  <label className="form-field"><span>{label} path</span><input list={id} value={draft} maxLength={1024} placeholder={placeholder} onChange={(e) => setDraft(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter" && !e.nativeEvent.isComposing) { e.preventDefault(); add(); } }} /></label>
  <datalist id={id}>{choices.map((path) => <option key={path} value={path} />)}</datalist>
  <button type="button" className="secondary-button" disabled={!draft.trim() || paths.includes(draft.trim())} onClick={add}>Add {label.toLowerCase()} path</button>
  {paths.length ? <ul className="frozen-files">{paths.map((path) => <li key={path}><code>{path}</code><button type="button" className="text-action" aria-label={`Remove ${path}`} onClick={() => onChange(paths.filter((p) => p !== path))}>Remove</button></li>)}</ul> : <p className="form-hint">None selected.</p>}
 </fieldset>;
}
