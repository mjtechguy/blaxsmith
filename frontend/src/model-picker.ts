/*
 * Model ordering and search ranking adapted from t3code
 * (https://github.com/pingdotgg/t3code, commit b2b43bef73):
 * apps/web/src/modelOrdering.ts, apps/web/src/components/chat/modelPickerSearch.ts,
 * and packages/shared/src/searchRanking.ts, rewritten without Effect.
 *
 * MIT License
 *
 * Copyright (c) 2026 T3 Tools Inc.
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

// --- searchRanking.ts -------------------------------------------------------

export function normalizeSearchQuery(input: string): string {
  return input.trim().toLowerCase();
}

export function scoreSubsequenceMatch(value: string, query: string): number | null {
  if (!query) return 0;
  let queryIndex = 0;
  let firstMatchIndex = -1;
  let previousMatchIndex = -1;
  let gapPenalty = 0;
  for (let valueIndex = 0; valueIndex < value.length; valueIndex += 1) {
    if (value[valueIndex] !== query[queryIndex]) continue;
    if (firstMatchIndex === -1) firstMatchIndex = valueIndex;
    if (previousMatchIndex !== -1) gapPenalty += valueIndex - previousMatchIndex - 1;
    previousMatchIndex = valueIndex;
    queryIndex += 1;
    if (queryIndex === query.length) {
      const spanPenalty = valueIndex - firstMatchIndex + 1 - query.length;
      const lengthPenalty = Math.min(64, value.length - query.length);
      return firstMatchIndex * 2 + gapPenalty * 3 + spanPenalty + lengthPenalty;
    }
  }
  return null;
}

const lengthPenalty = (value: string, query: string) => Math.min(64, Math.max(0, value.length - query.length));

function findBoundaryMatchIndex(value: string, query: string, markers: readonly string[]): number | null {
  let best: number | null = null;
  for (const marker of markers) {
    const index = value.indexOf(`${marker}${query}`);
    if (index === -1) continue;
    const matchIndex = index + marker.length;
    if (best === null || matchIndex < best) best = matchIndex;
  }
  return best;
}

// Tiered match: exact, prefix, word boundary, substring, then fuzzy. Inputs
// must already be normalized.
export function scoreQueryMatch(input: {
  value: string; query: string; exactBase: number; prefixBase?: number; boundaryBase?: number; includesBase?: number; fuzzyBase?: number;
}): number | null {
  const { value, query } = input;
  if (!value || !query) return null;
  if (value === query) return input.exactBase;
  if (input.prefixBase !== undefined && value.startsWith(query)) return input.prefixBase + lengthPenalty(value, query);
  if (input.boundaryBase !== undefined) {
    const index = findBoundaryMatchIndex(value, query, [" ", "-", "_", "/", "."]);
    if (index !== null) return input.boundaryBase + index * 2 + lengthPenalty(value, query);
  }
  if (input.includesBase !== undefined) {
    const index = value.indexOf(query);
    if (index !== -1) return input.includesBase + index * 2 + lengthPenalty(value, query);
  }
  if (input.fuzzyBase !== undefined) {
    const fuzzy = scoreSubsequenceMatch(value, query);
    if (fuzzy !== null) return input.fuzzyBase + fuzzy;
  }
  return null;
}

// --- modelPickerSearch.ts ---------------------------------------------------

export type ModelPickerSearchableModel = {
  driverKind: string; // Provider, e.g. "anthropic", so "anthropic" matches every Anthropic connection.
  providerDisplayName: string; // Connection label.
  name: string;
  shortName?: string; // The model id when it differs from the name.
  isFavorite?: boolean;
};

const FAVORITE_SCORE_BOOST = 24;

export function buildModelPickerSearchText(model: ModelPickerSearchableModel): string {
  return normalizeSearchQuery([model.name, model.shortName, model.driverKind, model.providerDisplayName].filter(Boolean).join(" "));
}

export function scoreModelPickerSearch(model: ModelPickerSearchableModel, query: string): number | null {
  const tokens = normalizeSearchQuery(query).split(/\s+/u).filter(Boolean);
  if (!tokens.length) return 0;
  const fields = [normalizeSearchQuery(model.name), ...(model.shortName ? [normalizeSearchQuery(model.shortName)] : []),
    normalizeSearchQuery(model.driverKind), normalizeSearchQuery(model.providerDisplayName), buildModelPickerSearchText(model)];
  let score = 0;
  for (const token of tokens) {
    const scores: number[] = [];
    fields.forEach((field, index) => {
      const base = index * 10;
      const s = scoreQueryMatch({ value: field, query: token, exactBase: base, prefixBase: base + 2, boundaryBase: base + 4,
        includesBase: base + 6, ...(token.length >= 3 ? { fuzzyBase: base + 100 } : {}) });
      if (s !== null) scores.push(s);
    });
    if (!scores.length) return null;
    score += Math.min(...scores);
  }
  return model.isFavorite ? score - FAVORITE_SCORE_BOOST : score;
}

// --- modelOrdering.ts -------------------------------------------------------

export type ProviderModelItem = { instanceId: string; slug: string };

export const providerModelKey = (instanceId: string, slug: string) => `${instanceId}:${slug}`;

const rankByValue = (values: readonly string[]) => new Map(values.map((value, index) => [value, index] as const));
const rank = (map: Map<string, number>, key: string) => map.get(key) ?? Number.POSITIVE_INFINITY;

// Favourites first (when grouped), then an explicit order, then the original order.
export function sortModelsForProviderInstance<T extends { slug: string }>(models: readonly T[], options: {
  modelOrder?: readonly string[]; favoriteModels?: ReadonlySet<string>; groupFavorites?: boolean;
} = {}): T[] {
  const order = rankByValue(options.modelOrder ?? []);
  const original = rankByValue(models.map((m) => m.slug));
  const favorites = options.favoriteModels ?? new Set<string>();
  return [...models].sort((a, b) =>
    (options.groupFavorites ? Number(favorites.has(b.slug)) - Number(favorites.has(a.slug)) : 0) ||
    rank(order, a.slug) - rank(order, b.slug) || rank(original, a.slug) - rank(original, b.slug));
}

export function sortProviderModelItems<T extends ProviderModelItem>(items: readonly T[], options: {
  favoriteModelKeys?: ReadonlySet<string>; groupFavorites?: boolean; instanceOrder?: readonly string[];
} = {}): T[] {
  const favorites = options.favoriteModelKeys ?? new Set<string>();
  const instances = rankByValue(options.instanceOrder ?? []);
  const original = rankByValue(items.map((i) => providerModelKey(i.instanceId, i.slug)));
  const key = (i: T) => providerModelKey(i.instanceId, i.slug);
  return [...items].sort((a, b) =>
    (options.groupFavorites ? Number(favorites.has(key(b))) - Number(favorites.has(key(a))) : 0) ||
    rank(instances, a.instanceId) - rank(instances, b.instanceId) || rank(original, key(a)) - rank(original, key(b)));
}

// --- Blaxsmith picker grouping ------------------------------------------------

export type PickerModel = ProviderModelItem & {
  name: string; connectionLabel: string; provider: string; recommended: boolean; legacy: boolean; isDefault: boolean; badge: string;
};
export type PickerGroup = { id: string; label: string; models: PickerModel[] };

// Recommended (org-pinned) first, then favourites, then one group per
// connection in connection order. Legacy models stay hidden unless asked for
// or currently selected. A query ranks across groups by search score.
export function pickerGroups(models: readonly PickerModel[], options: {
  favorites: ReadonlySet<string>; showLegacy: boolean; query: string; selected?: string; connectionOrder: readonly string[];
}): PickerGroup[] {
  const visible = models.filter((m) => options.showLegacy || !m.legacy || providerModelKey(m.instanceId, m.slug) === options.selected);
  const searchable = (m: PickerModel): ModelPickerSearchableModel => ({ driverKind: m.provider, providerDisplayName: m.connectionLabel,
    name: m.name, shortName: m.slug !== m.name ? m.slug : undefined, isFavorite: options.favorites.has(providerModelKey(m.instanceId, m.slug)) });
  if (options.query.trim()) {
    const ranked = visible.map((m, index) => ({ m, index, score: scoreModelPickerSearch(searchable(m), options.query) }))
      .filter((x): x is { m: PickerModel; index: number; score: number } => x.score !== null)
      .sort((a, b) => a.score - b.score || a.index - b.index).map((x) => x.m);
    return ranked.length ? [{ id: "matches", label: "Matches", models: ranked }] : [];
  }
  const key = (m: PickerModel) => providerModelKey(m.instanceId, m.slug);
  const ordered = sortProviderModelItems(visible, { instanceOrder: options.connectionOrder });
  const recommended = ordered.filter((m) => m.recommended);
  const favorites = ordered.filter((m) => !m.recommended && options.favorites.has(key(m)));
  const taken = new Set([...recommended, ...favorites].map(key));
  const groups: PickerGroup[] = [];
  if (recommended.length) groups.push({ id: "recommended", label: "Recommended", models: recommended });
  if (favorites.length) groups.push({ id: "favorites", label: "Favourites", models: favorites });
  for (const instanceId of options.connectionOrder) {
    const rest = ordered.filter((m) => m.instanceId === instanceId && !taken.has(key(m)));
    const defaults = rest.filter((m) => m.isDefault).map((m) => m.slug);
    if (rest.length) groups.push({ id: instanceId, label: rest[0].connectionLabel, models: sortModelsForProviderInstance(rest, { modelOrder: defaults }) });
  }
  return groups;
}
