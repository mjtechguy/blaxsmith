import { useState } from "react";
import type { ConnectionModel } from "./gen/blaxsmith/api/v1/connections_pb";
import type { PickerModel } from "./model-picker";
import { providerModelKey } from "./model-picker";

export type AdvicePhase = "planning" | "implementation" | "review";
export type AdvicePreference = "preserve" | "balanced" | "quality";
export function phaseGuidance(phase: AdvicePhase, preference: AdvicePreference, ambiguous: boolean, crossComponent: boolean, highRisk: boolean, weakChecks: boolean) {
 const difficult = ambiguous || crossComponent || highRisk || weakChecks;
 const tier = highRisk || (difficult && (preference === "quality" || phase !== "implementation")) ? "deep" : difficult || preference === "quality" || phase !== "implementation" ? "capable" : "focused";
 const effort = difficult ? "high" : preference === "preserve" ? "low" : "medium";
 const reasons = [phase === "planning" ? "Planning needs a coherent view of requirements, existing code and tradeoffs." : phase === "review" ? "Independent review needs contract, caller and failure-path analysis." : "A bounded implementation packet with clear acceptance is suitable for focused work.",
  ...(ambiguous ? ["Requirements still contain ambiguity."] : []), ...(crossComponent ? ["Several components or contracts must stay consistent."] : []), ...(highRisk ? ["Mistakes have high consequences; evaluate a stronger model on representative work."] : []), ...(weakChecks ? ["Automated checks give limited feedback; strengthen the evidence before relying on repeated cheap attempts."] : []),
  preference === "preserve" ? "Preserve allowance: reduce effort for clear work and measure retries before increasing it." : preference === "quality" ? "Quality first: favor capability where useful; maximum reasoning is still not selected automatically." : "Balanced: begin with moderate effort and adjust from observed outcomes."];
 return { tier, effort, reasons };
}

// Curated capability positioning, not benchmark scores, prices or private
// account availability. Exact IDs only; no inference from a model-name suffix.
const evidence: Record<string, { tier: string; url: string }> = {
 "gpt-6-luna": { tier: "focused", url: "https://developers.openai.com/api/docs/models/gpt-6-luna" },
 "gpt-6-sol": { tier: "capable", url: "https://developers.openai.com/api/docs/models/gpt-6-sol" },
 "gpt-6-astra": { tier: "deep", url: "https://developers.openai.com/api/docs/models/gpt-6-astra" },
};
const reviewedAt = "2026-09-26";

export function ModelAdvice({ phase, models, info, checkedAt, onApply }: { phase: AdvicePhase; models: PickerModel[]; info: Map<string, ConnectionModel>; checkedAt: string[]; onApply: (model: PickerModel, effort: string | undefined) => void }) {
 const [preference, setPreference] = useState<AdvicePreference>("balanced");
 const [ambiguous, setAmbiguous] = useState(false); const [cross, setCross] = useState(false); const [risk, setRisk] = useState(false); const [weak, setWeak] = useState(false);
 const advice = phaseGuidance(phase, preference, ambiguous, cross, risk, weak);
 const stale = Date.now() - Date.parse(`${reviewedAt}T00:00:00Z`) > 90 * 86_400_000;
 const candidates = models.filter((m) => m.provider === "openai" && !m.legacy && evidence[m.slug]?.tier === advice.tier);
 const proposed = stale ? undefined : candidates.find((m) => m.recommended) ?? candidates.find((m) => m.isDefault) ?? candidates[0];
 const metadata = proposed ? info.get(providerModelKey(proposed.instanceId, proposed.slug)) : undefined;
 const effort = metadata?.efforts.includes(advice.effort) ? advice.effort : metadata?.defaultEffort && metadata.efforts.includes(metadata.defaultEffort) ? metadata.defaultEffort : undefined;
 return <details className="observation-provenance"><summary>Model guidance for {phase}</summary>
 <label className="form-field"><span>Model preference</span><select value={preference} onChange={(e) => setPreference(e.target.value as AdvicePreference)}><option value="preserve">Preserve allowance</option><option value="balanced">Balanced</option><option value="quality">Quality first</option></select></label>
 <fieldset><legend>Work characteristics</legend>{[["Ambiguous requirements", ambiguous, setAmbiguous], ["Several components or contracts", cross, setCross], ["High consequence errors", risk, setRisk], ["Limited automated verification", weak, setWeak]].map(([label, checked, change]) => <label key={String(label)} className="checkbox-field"><input type="checkbox" checked={Boolean(checked)} onChange={(e) => (change as (value: boolean) => void)(e.target.checked)} /><span>{String(label)}</span></label>)}</fieldset>
 <p><strong>Suggested capability: {advice.tier} · effort: {advice.effort}</strong></p><ul>{advice.reasons.map((reason) => <li key={reason}>{reason}</li>)}</ul>
 {proposed ? <><p>Listed candidate: <strong>{proposed.name}</strong> · {proposed.connectionLabel}{effort ? ` · supported effort: ${effort}` : " · effort support unknown; keep your explicit setting"}</p><button type="button" className="secondary-button" onClick={() => onApply(proposed, effort)}>Apply suggested model{effort ? " and effort" : ""}</button><p><a href={evidence[proposed.slug].url} target="_blank" rel="noreferrer">Provider capability description</a> · reviewed {reviewedAt}</p></> : <p>No fresh curated match is listed for this capability. Choose a connected model using team experience and representative evaluations; no alternative provider is selected automatically.</p>}
 <p>Applying pins the listed connection, model and supported effort for this phase. It never switches billing accounts automatically. Review the connection’s billing arrangement before launch. It never changes quality gates or starts work. All listed models, including other providers and open-weight endpoints, remain selectable.</p>
 <details><summary>Evidence and limitations</summary><p>This phase mapping is an Anvil heuristic, not a comparative benchmark ranking. Curated descriptions currently cover three OpenAI models; other model capabilities, prices, token use and subscription allowance are unknown here. Team-recommended/default metadata only breaks ties within a documented capability group.</p><p>Documentation reviewed {reviewedAt}{stale ? " · older than 90 days; model suggestions disabled pending review" : ""}. Connection catalog timestamps: {[...new Set(checkedAt.filter(Boolean))].join(", ") || "Unknown"}. Catalog listing does not establish current grants, approved runtime support or remaining quota; launch preflight rechecks them.</p><p>Compare exact model, harness/version, effort, prompts, tools and task fixtures. Measure accepted outcomes, repair attempts, elapsed time and total observed cost; API prices cannot establish subscription allowance.</p><ul>{Object.entries(evidence).map(([id, record]) => <li key={id}><a href={record.url} target="_blank" rel="noreferrer">{id}</a> · provider positioning · {record.tier}</li>)}</ul></details>
 </details>;
}
