export type PlanDecision = { id: string; question: string; choice: string; reason: string; authority: "user" | "proposal"; sources: string[]; alternatives?: string[] };
export type PlanUnknown = { id: string; description: string; disposition: "blocking" | "investigable" | "assumed" | "deferred"; reason: string; sources: string[] };
export type PlanDocument = {
 decisions?: PlanDecision[]; unknowns?: PlanUnknown[];
 schema_version: string; title: string; summary: string; assumptions: string[]; out_of_scope: string[]; open_questions: string[];
 requirements: { id: string; description: string; sources: string[]; examples: string[] }[];
 phases: { id: string; title: string; outcome: string }[];
 tasks: { id: string; title: string; phase: string; reason: string; instructions: string; depends_on: string[]; requirement_ids: string[]; acceptance: string[]; validation: string[] }[];
};

// Existing IDs stay stable. Structural changes still pass server reference and coverage checks.
// The server remains responsible for schema, reference and cycle validation.
const nextID = (prefix: string, items: { id: string }[]) => {
 const used = new Set(items.map((item) => item.id)); let n = 1; while (used.has(`${prefix}${n}`)) n++;
 return `${prefix}${n}`;
};

export function PlanEditor({ value, onChange, contextSources = [] }: { value: PlanDocument; onChange: (value: PlanDocument) => void; contextSources?: string[] }) {
 const sources = [...new Set(["brief", ...contextSources, ...value.requirements.flatMap((r) => r.sources), ...(value.decisions ?? []).flatMap((d) => d.sources), ...(value.unknowns ?? []).flatMap((u) => u.sources)])];
 const addTask = (phase: string) => onChange({ ...value, tasks: [...value.tasks, { id: nextID("T", value.tasks), title: "", phase, reason: "", instructions: "", depends_on: [], requirement_ids: [], acceptance: [""], validation: [] }] });
 return <div className="plan-editor">
  <label className="form-field"><span>Plan title</span><input required maxLength={300} value={value.title} onChange={(e) => onChange({ ...value, title: e.target.value })} /></label>
  <label className="form-field"><span>Plan summary</span><textarea required rows={3} maxLength={8000} value={value.summary} onChange={(e) => onChange({ ...value, summary: e.target.value })} /></label>
  <p className="form-hint">Edit the structure and assignments before implementation. Saving creates a new version. Removing a task also removes its dependency links. Every requirement needs an assignment and every phase needs a task before saving.</p>
  <PlanCoverage value={value} />
  {value.phases.map((phase, phaseIndex) => <section key={phase.id} className="plan-editor-phase"><h4>{phase.id} · {phase.title || "New phase"}</h4>
   <label className="form-field"><span>{phase.id} phase title</span><input required maxLength={300} value={phase.title} onChange={(e) => onChange({ ...value, phases: value.phases.map((p) => p.id === phase.id ? { ...p, title: e.target.value } : p) })} /></label>
   <label className="form-field"><span>{phase.id} outcome</span><textarea required rows={2} maxLength={4000} value={phase.outcome} onChange={(e) => onChange({ ...value, phases: value.phases.map((p) => p.id === phase.id ? { ...p, outcome: e.target.value } : p) })} /></label>
   <div className="editor-actions"><button type="button" className="text-action" disabled={phaseIndex === 0} onClick={() => { const phases = [...value.phases]; [phases[phaseIndex-1], phases[phaseIndex]] = [phases[phaseIndex], phases[phaseIndex-1]]; onChange({ ...value, phases }); }}>Move {phase.id} earlier</button>
   <button type="button" className="text-action" disabled={value.phases.length === 1 || value.tasks.some((t) => t.phase === phase.id)} onClick={() => onChange({ ...value, phases: value.phases.filter((p) => p.id !== phase.id) })}>Remove {phase.id} phase</button></div>
   <p className="form-hint">Move or remove this phase’s tasks before removing the phase.</p>
   {value.tasks.map((task, index) => {
    if (task.phase !== phase.id) return null;
    const update = (changes: Partial<PlanDocument["tasks"][number]>) => onChange({ ...value, tasks: value.tasks.map((t, i) => i === index ? { ...t, ...changes } : t) });
    return <details key={task.id} className="goal-plan-task"><summary><span>{task.id}</span> {task.title || "Untitled task"}</summary><div className="plan-editor-fields">
     <label className="form-field"><span>{task.id} title</span><input required maxLength={300} value={task.title} onChange={(e) => update({ title: e.target.value })} /></label>
     <label className="form-field"><span>{task.id} phase</span><select value={task.phase} onChange={(e) => update({ phase: e.target.value })}>{value.phases.map((p) => <option key={p.id} value={p.id}>{p.id} · {p.title || "New phase"}</option>)}</select></label>
     <button type="button" className="text-action" disabled={value.tasks.length === 1} onClick={() => onChange({ ...value, tasks: value.tasks.filter((t) => t.id !== task.id).map((t) => ({ ...t, depends_on: (t.depends_on ?? []).filter((id) => id !== task.id) })) })}>Remove {task.id} task</button>
     <label className="form-field"><span>{task.id} reasoning</span><textarea required rows={2} maxLength={4000} value={task.reason} onChange={(e) => update({ reason: e.target.value })} /></label>
     <label className="form-field"><span>{task.id} instructions</span><textarea required rows={6} maxLength={16000} value={task.instructions} onChange={(e) => update({ instructions: e.target.value })} /></label>
     <fieldset><legend>{task.id} dependencies</legend><p className="form-hint">These tasks must finish first. Cycles are rejected when saving.</p>
      {value.tasks.filter((other) => other.id !== task.id).map((other) => <label key={other.id} className="checkbox-field"><input type="checkbox" checked={task.depends_on?.includes(other.id) ?? false} onChange={(e) => update({ depends_on: e.target.checked ? [...(task.depends_on ?? []), other.id] : task.depends_on.filter((id) => id !== other.id) })} /><span>{other.id} · {other.title}</span></label>)}
      {value.tasks.length === 1 ? <p>No other tasks.</p> : null}
     </fieldset>
     <fieldset><legend>{task.id} requirements</legend>{value.requirements.map((r) => <label key={r.id} className="checkbox-field"><input type="checkbox" checked={task.requirement_ids.includes(r.id)} onChange={(e) => update({ requirement_ids: e.target.checked ? [...task.requirement_ids, r.id] : task.requirement_ids.filter((id) => id !== r.id) })} /><span>{r.id} · {r.description}</span></label>)}</fieldset>
     <PlanTextList label={`${task.id} acceptance criteria`} values={task.acceptance} min={1} max={16} onChange={(acceptance) => update({ acceptance })} />
     <PlanTextList label={`${task.id} suggested validation`} values={task.validation ?? []} min={0} max={16} onChange={(validation) => update({ validation })} />
     <p className="form-hint">Suggested validation does not change required project checks.</p>
    </div></details>;
   })}
   <button type="button" className="secondary-button" disabled={value.tasks.length >= 64} onClick={() => addTask(phase.id)}>Add task to {phase.id}</button>
  </section>)}
  <button type="button" className="secondary-button" disabled={value.phases.length >= 16} onClick={() => onChange({ ...value, phases: [...value.phases, { id: nextID("P", value.phases), title: "", outcome: "" }] })}>Add phase</button>
  <details className="observation-provenance"><summary>Edit requirements and examples</summary>{value.requirements.map((r, index) => {
   const update = (changes: Partial<PlanDocument["requirements"][number]>) => onChange({ ...value, requirements: value.requirements.map((item, i) => i === index ? { ...item, ...changes } : item) });
   return <section key={r.id} className="plan-editor-fields"><h4>{r.id}</h4>
    <fieldset><legend>{r.id} sources</legend>{sources.map((source) => <label key={source} className="checkbox-field"><input type="checkbox" checked={r.sources.includes(source)} onChange={(e) => update({ sources: e.target.checked ? [...r.sources, source] : r.sources.filter((s) => s !== source) })} /><span>{source}</span></label>)}</fieldset>
    <button type="button" className="text-action" disabled={value.requirements.length === 1 || value.tasks.some((t) => t.requirement_ids.includes(r.id))} onClick={() => onChange({ ...value, requirements: value.requirements.filter((item) => item.id !== r.id) })}>Remove {r.id} requirement</button>
    <p className="form-hint">Unassign this requirement from its tasks before removing it.</p>
    <label className="form-field"><span>{r.id} description</span><textarea required rows={3} maxLength={4000} value={r.description} onChange={(e) => update({ description: e.target.value })} /></label>
    <PlanTextList label={`${r.id} examples`} values={r.examples} min={1} max={16} onChange={(examples) => update({ examples })} />
   </section>;
  })}<button type="button" className="secondary-button" disabled={value.requirements.length >= 128} onClick={() => onChange({ ...value, requirements: [...value.requirements, { id: nextID("R", value.requirements), description: "", sources: ["brief"], examples: [""] }] })}>Add requirement</button></details>
  <PlanDecisionsEditor value={value} sources={sources} onChange={onChange} />
  <details className="observation-provenance"><summary>Edit assumptions, exclusions and open questions</summary>
   <PlanTextList label="Assumptions" values={value.assumptions ?? []} min={0} max={32} onChange={(assumptions) => onChange({ ...value, assumptions })} />
   <PlanTextList label="Out of scope" values={value.out_of_scope ?? []} min={0} max={32} onChange={(out_of_scope) => onChange({ ...value, out_of_scope })} />
   <PlanTextList label="Open questions" values={value.open_questions ?? []} min={0} max={32} onChange={(open_questions) => onChange({ ...value, open_questions })} />
  </details>
 </div>;
}

function PlanTextList({ label, values, min, max, onChange }: { label: string; values: string[]; min: number; max: number; onChange: (values: string[]) => void }) {
 return <fieldset className="plan-editor-list"><legend>{label}</legend>
  {values.map((text, index) => <div className="plan-editor-item" key={index}><label className="form-field"><span>{label} {index + 1}</span><textarea required rows={2} maxLength={4000} value={text} onChange={(e) => onChange(values.map((v, i) => i === index ? e.target.value : v))} /></label><button type="button" className="text-action" disabled={values.length <= min} aria-label={`Remove ${label} ${index + 1}`} onClick={() => onChange(values.filter((_, i) => i !== index))}>Remove</button></div>)}
  <button type="button" className="secondary-button" disabled={values.length >= max} onClick={() => onChange([...values, ""])}>Add {label.toLowerCase()} item</button>
 </fieldset>;
}

export function PlanCoverage({ value }: { value: PlanDocument }) {
 return <details className="observation-provenance"><summary>Requirement coverage · {value.requirements.filter((r) => value.tasks.some((t) => t.requirement_ids.includes(r.id))).length}/{value.requirements.length} assigned</summary>
  <p className="form-hint">Assignment coverage shows planned work, not proof of implementation or passing checks.</p>
  <ul>{value.requirements.map((r) => { const assigned = value.tasks.filter((t) => t.requirement_ids.includes(r.id)); return <li key={r.id}><strong>{r.id} · {r.description || "New requirement"}</strong><p>{assigned.length ? assigned.map((t) => `${t.id} · ${t.title}`).join("; ") : "Unassigned — choose an implementing task"}</p></li>; })}</ul>
 </details>;
}

// A conservative structural review aid. It does not infer code semantics or
// change execution/evidence state. Cyclic unsaved drafts still terminate.
function planImpact(before: PlanDocument, after: PlanDocument) {
 const changed = (a: unknown, b: unknown) => JSON.stringify(a) !== JSON.stringify(b);
 const changes = (a: { id: string }[], b: { id: string }[]) => {
  const old = new Map(a.map((v) => [v.id, v])); const next = new Map(b.map((v) => [v.id, v]));
  return new Set([...old.keys(), ...next.keys()].filter((id) => changed(old.get(id), next.get(id))));
 };
 const requirements = changes(before.requirements, after.requirements); const phases = changes(before.phases, after.phases);
 const tasks = changes(before.tasks, after.tasks); const reasons = new Map<string, Set<string>>();
 const mark = (id: string, reason: string) => { const entries = reasons.get(id) ?? new Set<string>(); entries.add(reason); reasons.set(id, entries); };
 const global = (["title", "summary", "assumptions", "out_of_scope", "open_questions", "decisions", "unknowns"] as const).some((key) => changed(before[key], after[key]));
 const allTasks = [...before.tasks, ...after.tasks];
 for (const task of allTasks) {
  if (global) mark(task.id, "Shared planning context changed");
  if (tasks.has(task.id)) mark(task.id, "Task assignment changed");
  if (phases.has(task.phase)) mark(task.id, `Phase ${task.phase} changed`);
  for (const id of task.requirement_ids) if (requirements.has(id)) mark(task.id, `Requirement ${id} changed`);
 }
 const queue = [...reasons.keys()]; const visited = new Set(queue);
 for (let i = 0; i < queue.length; i++) for (const task of allTasks) {
  if (!(task.depends_on ?? []).includes(queue[i])) continue;
  mark(task.id, `Depends on affected ${queue[i]}`);
  if (!visited.has(task.id)) { visited.add(task.id); queue.push(task.id); }
 }
 return [...after.tasks, ...before.tasks.filter((t) => !after.tasks.some((next) => next.id === t.id))].filter((t) => reasons.has(t.id)).map((task) => ({ task, removed: !after.tasks.some((t) => t.id === task.id), reasons: [...reasons.get(task.id)!] }));
}

export function PlanImpact({ before, after }: { before: PlanDocument; after: PlanDocument }) {
 const impact = planImpact(before, after);
 return <details className="observation-provenance plan-impact"><summary>Change impact · {impact.length} assignments need review</summary>
 <p>Requirement, phase and task edits include downstream dependencies from both versions. Shared planning context changes flag every assignment. This is a conservative structural estimate; it does not establish which code or checks remain valid.</p>
 {impact.length ? <ul>{impact.map(({ task, removed, reasons }) => <li key={task.id}><strong>{task.id} · {task.title}{removed ? " · removed" : ""}</strong><p>{reasons.join("; ")}</p></li>)}</ul> : <p>No assignment impact detected from the compared fields.</p>}
 <p>Saving retains all earlier plans and evidence. Active runs keep their frozen version. Review their candidate and checks before deciding to continue, pause or cancel; saving does not steer an active worker or transfer historical acceptance to this plan.</p>
 </details>;
}

export function PlanDiff({ before, after }: { before: PlanDocument; after: PlanDocument }) {
 const fields = ["title", "summary", "assumptions", "out_of_scope", "open_questions", "decisions", "unknowns"] as const;
 return <div className="plan-diff"><PlanImpact before={before} after={after} /><p>Changed content is shown in full. These are plan changes, not code or verification results.</p>
  {fields.filter((key) => JSON.stringify(before[key]) !== JSON.stringify(after[key])).map((key) => <details key={key}><summary>Changed {key.replaceAll("_", " ")}</summary><strong>Before</strong><pre>{JSON.stringify(before[key], null, 2)}</pre><strong>After</strong><pre>{JSON.stringify(after[key], null, 2)}</pre></details>)}
  {(["requirements", "phases", "tasks"] as const).map((key) => {
   const previous = new Map<string, { id: string }>(before[key].map((item) => [item.id, item]));
   const next = new Map<string, { id: string }>(after[key].map((item) => [item.id, item]));
   const ids = [...new Set([...previous.keys(), ...next.keys()])];
   return <section key={key}><h4>{key}</h4>{JSON.stringify([...previous.keys()]) !== JSON.stringify([...next.keys()]) ? <p>Order / membership: {[...previous.keys()].join(", ")} → {[...next.keys()].join(", ")}</p> : null}
    {ids.filter((id) => JSON.stringify(previous.get(id)) !== JSON.stringify(next.get(id))).map((id) => <details key={id}><summary>{!previous.has(id) ? "Added" : !next.has(id) ? "Removed" : "Changed"} {id}</summary>{previous.has(id) ? <><strong>Before</strong><pre>{JSON.stringify(previous.get(id), null, 2)}</pre></> : null}{next.has(id) ? <><strong>After</strong><pre>{JSON.stringify(next.get(id), null, 2)}</pre></> : null}</details>)}
   </section>;
  })}
 </div>;
}

export function PlanReadiness({ value }: { value: PlanDocument }) {
 const blocking = (value.unknowns ?? []).filter((u) => u.disposition === "blocking");
 const open = value.open_questions?.length ?? 0;
 return <section aria-label="Planning readiness summary"><h4>Planning readiness · {blocking.length + open ? `${blocking.length + open} unresolved questions` : "No declared blocking questions"}</h4><p>Completeness is based on the saved plan, not an independent evaluation. Choose whether unresolved questions block launch in execution settings.</p>
 {(value.decisions?.length ?? 0) > 0 ? <details className="observation-provenance"><summary>Decisions · {value.decisions?.length}</summary>{value.decisions?.map((d) => <div key={d.id}><h5>{d.id} · {d.question}</h5><p><strong>{d.authority === "user" ? "Attributed to user" : "Proposal"}:</strong> {d.choice}</p><p>{d.reason}</p><p>Sources: {d.sources.join(", ")}</p>{d.alternatives?.length ? <p>Alternatives considered: {d.alternatives.join("; ")}</p> : null}</div>)}</details> : null}
 {(value.unknowns?.length ?? 0) > 0 ? <details className="observation-provenance"><summary>Classified unknowns · {value.unknowns?.length}</summary><ul>{value.unknowns?.map((u) => <li key={u.id}><strong>{u.id} · {u.disposition}</strong><p>{u.description}</p><p>{u.reason}</p><p>Sources: {u.sources.join(", ")}</p></li>)}</ul></details> : null}
 </section>;
}
function PlanDecisionsEditor({ value, sources, onChange }: { value: PlanDocument; sources: string[]; onChange: (value: PlanDocument) => void }) {
 const decisions = value.decisions ?? []; const unknowns = value.unknowns ?? [];
 return <details className="observation-provenance"><summary>Edit decisions and classified unknowns</summary>
 <p>Record concise reasons and source references. Attributing a choice to the user requires a supporting answer or brief. Proposals and assumptions do not grant authority.</p>
 {decisions.map((d) => { const update = (patch: Partial<PlanDecision>) => onChange({ ...value, decisions: decisions.map((item) => item.id === d.id ? { ...item, ...patch } : item) }); return <fieldset key={d.id}><legend>{d.id} decision</legend>
 {(["question", "choice", "reason"] as const).map((key) => <label key={key} className="form-field"><span>{d.id} {key}</span><textarea required maxLength={4000} rows={2} value={d[key]} onChange={(e) => update({ [key]: e.target.value })} /></label>)}
 <label className="form-field"><span>{d.id} attribution</span><select value={d.authority} onChange={(e) => update({ authority: e.target.value as PlanDecision["authority"] })}><option value="proposal">Proposal</option><option value="user">Attributed to user</option></select></label>
 <PlanSources label={`${d.id} sources`} choices={sources} selected={d.sources} onChange={(sources) => update({ sources })} />
 <PlanTextList label={`${d.id} alternatives`} values={d.alternatives ?? []} min={0} max={8} onChange={(alternatives) => update({ alternatives })} />
 <button type="button" className="text-action" onClick={() => onChange({ ...value, decisions: decisions.filter((item) => item.id !== d.id) })}>Remove {d.id} decision</button>
 </fieldset>; })}
 <button type="button" className="secondary-button" disabled={decisions.length >= 32} onClick={() => onChange({ ...value, decisions: [...decisions, { id: nextID("D", decisions), question: "", choice: "", reason: "", authority: "proposal", sources: ["brief"], alternatives: [] }] })}>Add decision</button>
 {unknowns.map((u) => { const update = (patch: Partial<PlanUnknown>) => onChange({ ...value, unknowns: unknowns.map((item) => item.id === u.id ? { ...item, ...patch } : item) }); return <fieldset key={u.id}><legend>{u.id} unknown</legend>
 <label className="form-field"><span>{u.id} description</span><textarea required maxLength={4000} rows={2} value={u.description} onChange={(e) => update({ description: e.target.value })} /></label>
 <label className="form-field"><span>{u.id} classification</span><select value={u.disposition} onChange={(e) => update({ disposition: e.target.value as PlanUnknown["disposition"] })}>{["blocking", "investigable", "assumed", "deferred"].map((kind) => <option key={kind}>{kind}</option>)}</select></label>
 <label className="form-field"><span>{u.id} reason</span><textarea required maxLength={4000} rows={2} value={u.reason} onChange={(e) => update({ reason: e.target.value })} /></label>
 <PlanSources label={`${u.id} sources`} choices={sources} selected={u.sources} onChange={(sources) => update({ sources })} />
 <button type="button" className="text-action" onClick={() => onChange({ ...value, unknowns: unknowns.filter((item) => item.id !== u.id) })}>Remove {u.id} unknown</button>
 </fieldset>; })}
 <button type="button" className="secondary-button" disabled={unknowns.length >= 32} onClick={() => onChange({ ...value, unknowns: [...unknowns, { id: nextID("U", unknowns), description: "", disposition: "investigable", reason: "", sources: ["brief"] }] })}>Add unknown</button>
 </details>;
}
function PlanSources({ label, choices, selected, onChange }: { label: string; choices: string[]; selected: string[]; onChange: (value: string[]) => void }) {
 return <fieldset><legend>{label}</legend>{choices.map((source) => <label key={source} className="checkbox-field"><input type="checkbox" checked={selected.includes(source)} onChange={(e) => onChange(e.target.checked ? [...selected, source] : selected.filter((item) => item !== source))} /><span>{source}</span></label>)}</fieldset>;
}
