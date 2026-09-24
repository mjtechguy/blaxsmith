import { useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Star } from "lucide-react";
import { currentSession, sessionQueryKey } from "./auth";
import { connectionModelsKey, listConnectionModels } from "./connections";
import { TextField } from "./form-field";
import type { ConnectionModel } from "./gen/blaxsmith/api/v1/connections_pb";
import { pickerGroups, providerModelKey, type PickerModel } from "./model-picker";

const harnesses = [["", "Any harness"], ["codex", "Codex"], ["claude-code", "Claude Code"], ["opencode", "OpenCode"]] as const;
const modelId = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/;

export type PickerConnection = { id: string; label: string; provider: string };

// Favourite models are a per-user browser preference (connection:model keys).
function useFavorites(key: string): [Set<string>, (modelKey: string) => void] {
  const [favorites, setFavorites] = useState<Set<string>>(() => {
    try { const parsed = JSON.parse(localStorage.getItem(key) || "[]"); return new Set(Array.isArray(parsed) ? parsed : []); } catch { return new Set(); }
  });
  return [favorites, (modelKey) => {
    const next = new Set(favorites);
    if (next.has(modelKey)) next.delete(modelKey); else next.add(modelKey);
    setFavorites(next);
    try { localStorage.setItem(key, JSON.stringify([...next])); } catch { /* Storage unavailable. */ }
  }];
}

const optionText = (m: PickerModel, info?: ConnectionModel) =>
  `${m.name !== m.slug ? `${m.name} (${m.slug})` : m.slug}${info?.contextTokens ? ` · ${Math.round(info.contextTokens / 1000)}k` : ""}` +
  `${m.isDefault ? " · default" : ""}${m.badge ? ` · ${m.badge}` : ""}${m.legacy ? " · legacy" : ""}`;

// Models the connection(s) can actually use: searchable, recommended first,
// then favourites, then grouped by connection; legacy behind a toggle; free
// text only under Advanced. With `connections`, picking a model also picks
// its connection (onChange's second argument). harness fixes the harness
// filter; fieldId keeps element ids unique when several pickers share a page.
export function ModelSelect({ connectionId, connections, value, onChange, harness: fixedHarness, fieldId, error }: {
  connectionId: string; connections?: PickerConnection[]; value: string;
  onChange: (model: string, connectionId?: string, info?: ConnectionModel) => void;
  harness?: string; fieldId?: string; error?: string;
}) {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const [chosenHarness, setHarness] = useState("");
  const harness = fixedHarness ?? chosenHarness;
  const [search, setSearch] = useState("");
  const [showLegacy, setShowLegacy] = useState(false);
  const [favorites, toggleFavorite] = useFavorites(`blaxsmith-model-favourites:${org}:${session.data?.principalId ?? ""}`);
  const sources: PickerConnection[] = connections ?? (connectionId ? [{ id: connectionId, label: "Models", provider: "" }] : []);
  const queries = useQueries({ queries: sources.map((c) => ({ queryKey: connectionModelsKey(org, c.id, harness), enabled: Boolean(org),
    queryFn: ({ signal }: { signal: AbortSignal }) => listConnectionModels(c.id, harness, signal) })) });
  const pending = sources.length > 0 && queries.some((q) => q.isPending);
  const info = new Map<string, ConnectionModel>();
  const models: PickerModel[] = sources.flatMap((c, i) => (queries[i]?.data?.models ?? []).map((m) => {
    info.set(providerModelKey(c.id, m.id), m);
    return { instanceId: c.id, slug: m.id, name: m.displayName || m.id, connectionLabel: c.label, provider: c.provider,
      recommended: m.recommended, legacy: m.legacy, isDefault: m.isDefault, badge: m.badge };
  }));
  const selected = value ? providerModelKey(connectionId, value) : "";
  const listed = info.has(selected);
  const groups = pickerGroups(models, { favorites, showLegacy, query: search, selected, connectionOrder: sources.map((c) => c.id) });
  const legacyCount = models.filter((m) => m.legacy).length;
  const id = fieldId || connectionId || "none";
  const errors = sources.flatMap((c, i) => queries[i]?.data?.error ? [`${c.label}: ${queries[i]?.data?.error}`] : []);
  const choose = (key: string) => {
    if (!key) return onChange("", connectionId);
    const at = key.indexOf(":");
    onChange(key.slice(at + 1), key.slice(0, at), info.get(key));
  };
  return <div className="editor-form model-select">
    {sources.length && fixedHarness === undefined ? <div className="form-field"><label htmlFor={`harness-${id}`}>Harness</label>
      <select id={`harness-${id}`} value={harness} onChange={(event) => setHarness(event.target.value)}>
        {harnesses.map(([hid, name]) => <option key={hid} value={hid}>{name}</option>)}
      </select></div> : null}
    {models.length > 6 ? <TextField label="Search models" name={`model-search-${id}`} autoComplete="off" placeholder="Name, id, provider, or connection"
      value={search} onChange={setSearch} onBlur={() => {}} required={false} /> : null}
    {sources.length ? <div className="form-field"><label htmlFor={`model-${id}`}>Model</label>
      <div className="model-select-row">
        <select id={`model-${id}`} value={listed ? selected : ""} onChange={(event) => choose(event.target.value)} disabled={pending} aria-invalid={error ? true : undefined}>
          <option value="">{pending ? "Loading models…" : !models.length ? "No models available" : groups.length ? "Choose a model" : "No models match"}</option>
          {groups.map((g) => <optgroup key={g.id} label={g.label}>
            {g.models.map((m) => { const key = providerModelKey(m.instanceId, m.slug);
              return <option key={`${g.id}-${key}`} value={key}>{favorites.has(key) ? "★ " : ""}{optionText(m, info.get(key))}{g.id === "recommended" || g.id === "favorites" || g.id === "matches" ? ` — ${m.connectionLabel}` : ""}</option>; })}
          </optgroup>)}
          {listed && !groups.some((g) => g.models.some((m) => providerModelKey(m.instanceId, m.slug) === selected))
            ? <option value={selected}>{value}</option> : null}
        </select>
        {listed ? <button type="button" className="icon-button" aria-pressed={favorites.has(selected)} onClick={() => toggleFavorite(selected)}
          aria-label={favorites.has(selected) ? `Remove ${value} from favourites` : `Add ${value} to favourites`} title={favorites.has(selected) ? "Unfavourite" : "Favourite"}>
          <Star size={16} aria-hidden="true" className={favorites.has(selected) ? "is-favorite" : undefined} /></button> : null}
      </div>
      {legacyCount ? <label className="recipe-check"><input type="checkbox" checked={showLegacy} onChange={(event) => setShowLegacy(event.target.checked)} /> Show legacy models ({legacyCount})</label> : null}
      {errors.map((e) => <span key={e} className="form-field-error">{e}</span>)}
      {queries.some((q) => q.isError) ? <span className="form-field-error">Models could not be loaded.</span> : null}
      {error && listed ? <span className="form-field-error">{error}</span> : null}
    </div> : null}
    <details className="advanced-disclosure" open={!sources.length || (Boolean(value) && !listed && !pending) || undefined}><summary>Advanced: type a model id</summary>
      <TextField label="Model id" name={`model-free-text-${id}`} autoComplete="off" placeholder="Exact provider model id" value={listed ? "" : value} onChange={(v) => onChange(v, connectionId)} onBlur={() => {}} required={false}
        error={value && !listed && !modelId.test(value) ? "Use up to 128 letters, numbers, periods, underscores, slashes, or hyphens." : !listed ? error : undefined} />
    </details>
  </div>;
}
