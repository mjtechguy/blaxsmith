// Spend over time: stacked daily columns, one hue per series in the fixed
// categorical order (dataviz method: validated palette, 2px surface gaps,
// 4px rounded data-ends, hairline grid, legend + column tooltip + table view).
import { useEffect, useMemo, useRef, useState } from "react";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";
import { usd } from "./gateway";

export type SpendPoint = { day: string; key: string; cost: number };
export type Series = { key: string; label: string; slot: number };

// Five named series plus "Other": the slots validated together (adjacent CVD
// ΔE ≥ 8.4, normal-vision ≥ 19.3, light and dark). A sixth entity folds into
// Other rather than getting a generated hue.
export const MAX_NAMED_SERIES = 5;

export function daysBetween(from: string, to: string): string[] {
  const out: string[] = [];
  const end = Date.parse(`${to}T00:00:00Z`);
  for (let t = Date.parse(`${from}T00:00:00Z`); t <= end && out.length < 400; t += 86_400_000) out.push(new Date(t).toISOString().slice(0, 10));
  return out;
}

// Series are ranked by spend in the window; the top five keep their slot for
// this view, everything else is Other.
export function buildSeries(points: SpendPoint[], labels: Map<string, string>): { series: Series[]; slotOf: (key: string) => Series } {
  const totals = new Map<string, number>();
  for (const p of points) totals.set(p.key, (totals.get(p.key) ?? 0) + p.cost);
  const ranked = [...totals.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const named = ranked.slice(0, MAX_NAMED_SERIES).map(([key], i) => ({ key, label: labels.get(key) || key, slot: i + 1 }));
  const other: Series = { key: "__other", label: "Other", slot: 6 };
  const byKey = new Map(named.map((s) => [s.key, s]));
  const series = ranked.length > MAX_NAMED_SERIES ? [...named, other] : named;
  return { series, slotOf: (key) => byKey.get(key) ?? other };
}

export function niceMax(value: number): number {
  if (value <= 0) return 1;
  const magnitude = 10 ** Math.floor(Math.log10(value));
  for (const step of [1, 2, 2.5, 5, 10]) if (step * magnitude >= value) return step * magnitude;
  return 10 * magnitude;
}

const H = 260, PAD = { top: 12, right: 12, bottom: 28, left: 56 };
const dayLabel = (day: string) => new Date(`${day}T00:00:00Z`).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });

// A column's top segment gets the 4px rounded data-end; lower segments stay square.
function segmentPath(x: number, y: number, w: number, h: number, rounded: boolean): string {
  if (!rounded || h < 4) return `M${x},${y}h${w}v${h}h${-w}Z`;
  const r = Math.min(4, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

const tableFeatureSet = tableFeatures({});
type DayRow = { day: string };

export function SpendChart({ points, labels, from, to, title }: {
  points: SpendPoint[]; labels: Map<string, string>; from: string; to: string; title: string;
}) {
  const [hover, setHover] = useState<string | null>(null);
  const [showTable, setShowTable] = useState(false);
  // The viewBox follows the rendered width so text stays at its CSS size.
  const frame = useRef<HTMLDivElement>(null);
  const [W, setW] = useState(720);
  useEffect(() => {
    const el = frame.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(([entry]) => setW(Math.max(260, Math.round(entry.contentRect.width))));
    observer.observe(el);
    return () => observer.disconnect();
  }, [showTable]);
  const days = useMemo(() => daysBetween(from, to), [from, to]);
  const { series, slotOf } = useMemo(() => buildSeries(points, labels), [points, labels]);
  // day → series key → cost, with folded Other.
  const grid = useMemo(() => {
    const out = new Map<string, Map<string, number>>();
    for (const p of points) {
      const key = slotOf(p.key).key;
      const row = out.get(p.day) ?? new Map<string, number>();
      row.set(key, (row.get(key) ?? 0) + p.cost);
      out.set(p.day, row);
    }
    return out;
  }, [points, slotOf]);
  const dayTotal = (day: string) => [...(grid.get(day)?.values() ?? [])].reduce((a, b) => a + b, 0);
  const top = niceMax(Math.max(0, ...days.map(dayTotal)));
  const plotW = W - PAD.left - PAD.right, plotH = H - PAD.top - PAD.bottom;
  const band = plotW / Math.max(days.length, 1);
  const barW = Math.max(2, Math.min(24, band * 0.7));
  const y = (value: number) => PAD.top + plotH - (value / top) * plotH;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * top);
  const labelEvery = Math.max(1, Math.ceil(days.length / Math.max(2, Math.floor(plotW / 72))));

  const columns = useMemo<ColumnDef<typeof tableFeatureSet, DayRow>[]>(() => [
    { id: "day", header: "Day", cell: ({ row }) => dayLabel(row.original.day) },
    ...series.map((s): ColumnDef<typeof tableFeatureSet, DayRow> => ({ id: s.key, header: s.label, cell: ({ row }) => usd(grid.get(row.original.day)?.get(s.key) ?? 0) })),
    { id: "total", header: "Total", cell: ({ row }) => <strong>{usd(dayTotal(row.original.day))}</strong> },
    // eslint-disable-next-line react-hooks/exhaustive-deps
  ], [series, grid]);
  const rows = useMemo(() => days.filter((d) => dayTotal(d) > 0).reverse().map((day) => ({ day })),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [days, grid]);
  const table = useTable({ features: tableFeatureSet, data: rows, columns, getRowId: (r) => r.day });

  const hovered = hover ? days.indexOf(hover) : -1;
  const tipRows = hover ? series.map((s) => ({ s, value: grid.get(hover)?.get(s.key) ?? 0 })).filter((r) => r.value > 0) : [];
  const tipW = 188, tipH = 30 + tipRows.length * 18;
  const tipX = hovered < 0 ? 0 : Math.min(W - tipW - 4, Math.max(4, PAD.left + hovered * band + band / 2 + 12 - (hovered > days.length / 2 ? tipW + 24 : 0)));

  return <figure className="viz-chart" aria-label={title}>
    {series.length > 1 ? <ul className="viz-legend" aria-label="Series">{series.map((s) => <li key={s.key}><span className={`viz-swatch viz-s${s.slot}`} aria-hidden="true" />{s.label}</li>)}</ul> : null}
    {!showTable ? <div ref={frame}><svg viewBox={`0 0 ${W} ${H}`} className="viz-svg" role="img" aria-label={`${title}. Use the table view for exact values.`} onPointerLeave={() => setHover(null)}>
      {ticks.map((t) => <g key={t} className="viz-grid">
        <line x1={PAD.left} x2={W - PAD.right} y1={y(t)} y2={y(t)} className={t === 0 ? "viz-baseline" : undefined} />
        <text x={PAD.left - 8} y={y(t)} dy="0.32em" textAnchor="end" className="viz-tick">{usd(t)}</text>
      </g>)}
      {days.map((day, i) => i % labelEvery === 0 ? <text key={day} x={PAD.left + i * band + band / 2} y={H - 8} textAnchor="middle" className="viz-tick">{dayLabel(day)}</text> : null)}
      {days.map((day, i) => {
        const x = PAD.left + i * band + (band - barW) / 2;
        const segments = series.map((s) => ({ s, value: grid.get(day)?.get(s.key) ?? 0 })).filter((seg) => seg.value > 0);
        let base = 0;
        const total = dayTotal(day);
        return <g key={day} className={`viz-column${hover === day ? " is-hover" : ""}`} tabIndex={total > 0 ? 0 : -1}
          aria-label={`${dayLabel(day)}: ${usd(total)} estimated`} onPointerEnter={() => setHover(day)} onFocus={() => setHover(day)} onBlur={() => setHover(null)}>
          <rect x={PAD.left + i * band} y={PAD.top} width={band} height={plotH} className="viz-hit" />
          {segments.map((seg, j) => {
            const y0 = y(base), y1 = y(base + seg.value);
            base += seg.value;
            // 2px surface gap between stacked segments.
            const h = Math.max(0, y0 - y1 - (j > 0 ? 2 : 0));
            return <path key={seg.s.key} d={segmentPath(x, y1, barW, h, j === segments.length - 1)} className={`viz-mark viz-s${seg.s.slot}`} />;
          })}
        </g>;
      })}
      {hover && tipRows.length ? <g className="viz-tooltip" transform={`translate(${tipX},${PAD.top + 4})`} pointerEvents="none">
        <rect width={tipW} height={tipH} rx={6} className="viz-tooltip-box" />
        <text x={10} y={18} className="viz-tooltip-title">{dayLabel(hover)} · <tspan className="viz-tooltip-value">{usd(dayTotal(hover))}</tspan></text>
        {tipRows.map((r, k) => <g key={r.s.key} transform={`translate(10,${36 + k * 18})`}>
          <line x1={0} x2={10} y1={-4} y2={-4} className={`viz-key viz-s${r.s.slot}`} />
          <text x={16} y={0} className="viz-tooltip-value">{usd(r.value)}</text>
          <text x={80} y={0} className="viz-tooltip-label">{r.s.label.length > 16 ? `${r.s.label.slice(0, 15)}…` : r.s.label}</text>
        </g>)}
      </g> : null}
    </svg></div> : <DataTable table={table} label={`${title} by day`} empty="No spend in this period." />}
    <figcaption className="viz-caption"><span>Estimated USD from list or contracted rates, by UTC day.</span>
      <button type="button" className="text-action" aria-pressed={showTable} onClick={() => setShowTable(!showTable)}>{showTable ? "Show chart" : "Show table"}</button></figcaption>
  </figure>;
}
