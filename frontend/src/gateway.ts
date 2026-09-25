// Model gateway client (docs/model-gateway-plan.md): settings, usage and
// run cost. Every cost is an estimate from list or contracted rates.
import { createClient } from "@connectrpc/connect";
import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { browserTransport, csrfToken, currentSession, sessionQueryKey } from "./auth";
import { GatewayAdminService, UsageService, type GatewayPool, type GatewayRoute, type GatewaySettings, type ModelPrice, type UsageTotals } from "./gen/blaxsmith/api/v1/gateway_pb";

const admin = createClient(GatewayAdminService, browserTransport);
const usage = createClient(UsageService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export const gatewayStatusKey = (org: string) => ["gateway-status", org] as const;
export const gatewaySettingsKey = (org: string) => ["gateway-settings", org] as const;
export const usageOverviewKey = (org: string, days: number, seriesBy: string) => ["gateway-overview", org, days, seriesBy] as const;
export const myUsageKey = (scope: string, days: number) => ["my-usage", scope, days] as const;
export const runCostKey = (scope: string, runId: string) => ["run-cost", scope, runId] as const;
export const pricesKey = (org: string) => ["gateway-prices", org] as const;
export const projectDeliveryKey = (org: string, projectId: string) => ["project-delivery", org, projectId] as const;

export const getGatewayStatus = (signal?: AbortSignal) => usage.getGatewayStatus({}, { signal });
export const getGatewaySettings = (signal?: AbortSignal) => admin.getGatewaySettings({}, { signal });
export const updateGatewaySettings = async (settings: Omit<GatewaySettings, "$typeName" | "$unknown">, expectedVersion: bigint) =>
  admin.updateGatewaySettings({ settings, expectedVersion }, await csrf());
export const getUsageOverview = (days: number, seriesBy: string, signal?: AbortSignal) => admin.getUsageOverview({ days, seriesBy }, { signal });
export const listModelPrices = (signal?: AbortSignal) => admin.listModelPrices({}, { signal });
export const setModelPriceOverride = async (price: Pick<ModelPrice, "provider" | "model" | "inputMicrosPerMtok" | "outputMicrosPerMtok" | "cacheReadMicrosPerMtok" | "cacheWriteMicrosPerMtok">) =>
  admin.setModelPriceOverride({ price }, await csrf());
export const getMyUsage = (days: number, signal?: AbortSignal) => usage.getMyUsage({ days }, { signal });
export const getRunCost = (runId: string, signal?: AbortSignal) => usage.getRunCost({ runId }, { signal });
export const getProjectDelivery = (projectId: string, signal?: AbortSignal) => usage.getProjectDelivery({ projectId }, { signal });
export const setProjectDelivery = async (projectId: string, deliveryMode: string) => usage.setProjectDelivery({ projectId, deliveryMode }, await csrf());

// The master switch gates every gateway surface except its settings page.
// Signed-out or failed reads count as off, so nothing appears by accident.
export function useGatewayEnabled(): boolean {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId ?? "";
  const status = useQuery({ queryKey: gatewayStatusKey(org), enabled: Boolean(org), staleTime: 60_000,
    queryFn: ({ signal }) => getGatewayStatus(signal) });
  return Boolean(org && status.data?.enabled);
}

const toNumber = (value: bigint | number) => typeof value === "bigint" ? Number(value) : value;

// "$1.23"; sub-cent amounts keep two significant digits so a small run is not "$0.00".
export function usd(micros: bigint | number): string {
  const dollars = toNumber(micros) / 1e6;
  if (dollars === 0) return "$0.00";
  if (Math.abs(dollars) < 0.01) return `$${dollars.toPrecision(2)}`;
  if (Math.abs(dollars) >= 10_000) return `$${compact(dollars)}`;
  return dollars.toLocaleString(undefined, { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

export function compact(value: bigint | number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(toNumber(value));
}

export function totalTokens(t?: UsageTotals): number {
  if (!t) return 0;
  return toNumber(t.inputTokens) + toNumber(t.outputTokens) + toNumber(t.cacheReadTokens) + toNumber(t.cacheWriteTokens);
}

export function percent(part: number, whole: number): string {
  if (!whole) return "0%";
  const value = (part / whole) * 100;
  return `${value < 10 && value > 0 ? value.toFixed(1) : Math.round(value)}%`;
}

// Per-MTok rate from micros: "$4.00 / MTok".
export const rate = (microsPerMTok: bigint | number) => `${usd(microsPerMTok)}`;

export const deliveryLabels: Record<string, string> = {
  native_raw: "Direct key (native_raw)",
  brokered_gateway: "Model gateway (brokered_gateway)",
};

export const routeKindLabels: Record<string, string> = {
  anthropic: "Anthropic", openai: "OpenAI", opencode_zen: "OpenCode Zen", opencode_go: "OpenCode Go",
  bedrock: "Amazon Bedrock", vertex: "Google Vertex AI", azure_openai: "Azure OpenAI", personal_subscription: "Personal subscription",
};

// Routes & pools (docs/model-gateway-plan.md §4, §5): API-key and cloud
// routes an organization owns. Personal subscriptions are never routes.
export const gatewayRoutesKey = (org: string) => ["gateway-routes", org] as const;
export const mySubscriptionLimitsKey = (scope: string) => ["my-subscription-limits", scope] as const;
export const listGatewayRoutes = (signal?: AbortSignal) => admin.listGatewayRoutes({}, { signal });
export const saveGatewayRoute = async (route: Partial<Omit<GatewayRoute, "$typeName" | "$unknown">>, cloudCredential = "") =>
  admin.saveGatewayRoute({ route, cloudCredential }, await csrf());
export const setGatewayRouteState = async (id: string, state: string) => admin.setGatewayRouteState({ id, state }, await csrf());
export const saveGatewayPool = async (pool: Partial<Omit<GatewayPool, "$typeName" | "$unknown">>) => admin.saveGatewayPool({ pool }, await csrf());
export const poolDetailKey = (org: string, poolId: string, hours: number) => ["gateway-pool-detail", org, poolId, hours] as const;
export const getGatewayPoolDetail = (poolId: string, hours: number, signal?: AbortSignal) => admin.getGatewayPoolDetail({ poolId, hours }, { signal });
export const listMySubscriptionLimits =(signal?: AbortSignal) => usage.listMySubscriptionLimits({}, { signal });

export const familyLabels: Record<string, string> = { anthropic: "Claude (Anthropic)", openai: "OpenAI", opencode: "OpenCode Zen", "opencode-go": "OpenCode Go" };
export const strategyLabels: Record<string, string> = { priority_headroom: "Priority, then headroom", weighted: "Weighted", fill_first: "Fill first" };
// Which route kinds serve a pool family; the server enforces the same.
export const familyKinds: Record<string, string[]> = { anthropic: ["anthropic", "bedrock", "vertex"], openai: ["openai", "azure_openai"], opencode: ["opencode_zen"], "opencode-go": ["opencode_go"] };

// "42 s" style countdown to an RFC 3339 time; "" when unknown or past.
export function countdown(at: string, now: number): string {
  if (!at) return "";
  const seconds = Math.ceil((Date.parse(at) - now) / 1000);
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  if (seconds < 90) return `${seconds} s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 90) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} h ${minutes % 60} min`;
  return `${Math.round(hours / 24)} d`;
}

// Window names as the owner sees them: primary (Codex), claude_5h, …
export function windowLabel(name: string, minutes: number): string {
  const span = !minutes ? "" : minutes % 1440 === 0 ? `${minutes / 1440}-day` : minutes % 60 === 0 ? `${minutes / 60}-hour` : `${minutes}-minute`;
  const base = name === "primary" ? "Primary" : name === "secondary" ? "Secondary" : name.replace(/^claude_/, "").replaceAll("_", " ");
  return span ? `${base} (${span} window)` : base;
}

// A clock that ticks once a second while mounted, for reset countdowns.
export function useNow(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  return now;
}
