// Model gateway G3 client (docs/model-gateway-plan.md §8, §9): soft budgets,
// threshold alerts and Project → Usage. Budgets are monthly (UTC) estimated
// USD; alerts never block anything.
import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { GatewayAdminService, UsageService, type Budget, type BudgetInput } from "./gen/blaxsmith/api/v1/gateway_pb";

const admin = createClient(GatewayAdminService, browserTransport);
const usage = createClient(UsageService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export const budgetsKey = (org: string) => ["gateway-budgets", org] as const;
export const budgetAlertsKey = (org: string, openOnly: boolean) => ["gateway-alerts", org, openOnly] as const;
export const projectUsageKey = (org: string, projectId: string) => ["project-usage", org, projectId] as const;

type Input = Omit<BudgetInput, "$typeName" | "$unknown">;

export const listBudgets = (signal?: AbortSignal) => admin.listBudgets({}, { signal });
export const setBudgetsEnabled = async (enabled: boolean) => admin.setBudgetsEnabled({ enabled }, await csrf());
export const createBudget = async (budget: Input) => admin.createBudget({ budget }, await csrf());
export const updateBudget = async (budgetId: string, budget: Input, expectedVersion: bigint) => admin.updateBudget({ budgetId, budget, expectedVersion }, await csrf());
export const archiveBudget = async (budgetId: string) => admin.archiveBudget({ budgetId }, await csrf());
export const listBudgetAlerts = (openOnly: boolean, signal?: AbortSignal) => admin.listBudgetAlerts({ openOnly }, { signal });
export const getProjectUsage = (projectId: string, signal?: AbortSignal) => usage.getProjectUsage({ projectId }, { signal });
export const acknowledgeBudgetAlert = async (alertId: string) => usage.acknowledgeBudgetAlert({ alertId }, await csrf());
export const snoozeBudgetAlert = async (alertId: string, hours: number) => usage.snoozeBudgetAlert({ alertId, hours }, await csrf());

export const scopeLabels: Record<string, string> = { organization: "Organization", project: "Project", user: "User" };

// Spend as a share of the budget, 0–1 and uncapped (a budget can be overspent).
export function budgetRatio(spend: bigint | number, amount: bigint | number): number {
  const whole = Number(amount);
  return whole > 0 ? Number(spend) / whole : 0;
}

// "attention" from the first threshold that fired, "danger" at or past 100%.
export function budgetTone(b: Pick<Budget, "spendUsdMicros" | "amountUsdMicros" | "thresholds">): "ok" | "attention" | "danger" {
  const pct = budgetRatio(b.spendUsdMicros, b.amountUsdMicros) * 100;
  if (pct >= 100) return "danger";
  const first = Math.min(...(b.thresholds.length ? b.thresholds : [80]));
  return pct >= first ? "attention" : "ok";
}

// "50, 80, 100" → [50, 80, 100]; null when not 1–6 distinct whole percents in 1–200.
export function parseThresholds(text: string): number[] | null {
  const parts = text.split(/[\s,]+/).filter(Boolean);
  if (!parts.length) return [50, 80, 100];
  const values = parts.map(Number);
  if (values.length > 6 || values.some((v) => !Number.isInteger(v) || v < 1 || v > 200) || new Set(values).size !== values.length) return null;
  return values.sort((a, b) => a - b);
}

// "$1,250.50" or "1250.5" → micros; null when not a positive amount up to $100M.
export function dollarsToMicros(text: string): bigint | null {
  const n = Number(text.replace(/[$,\s]/g, ""));
  return text.trim() && Number.isFinite(n) && n > 0 && n <= 100_000_000 ? BigInt(Math.round(n * 1e6)) : null;
}
