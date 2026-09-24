// Model gateway client (docs/model-gateway-plan.md): settings, usage and
// run cost. Every cost is an estimate from list or contracted rates.
import { createClient } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { browserTransport, csrfToken, currentSession, sessionQueryKey } from "./auth";
import { GatewayAdminService, UsageService, type GatewaySettings, type ModelPrice, type UsageTotals } from "./gen/blaxsmith/api/v1/gateway_pb";

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
};
