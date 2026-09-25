import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { currentSession, sessionQueryKey } from "../auth";
import { compact, countdown, getMyUsage, listMySubscriptionLimits, mySubscriptionLimitsKey, myUsageKey, totalTokens, usd, useNow, windowLabel } from "../gateway";
import type { SubscriptionLimits } from "../gen/blaxsmith/api/v1/gateway_pb";
import { DashboardLayout } from "../layouts";
import { Card, sentence, StatePanel, StatTile, Timestamp } from "../ui";
import { GatewayOff, RangeFilter, SliceTable } from "../usage-ui";

type Search = { days?: number };

export const Route = createFileRoute("/me/usage")({
  component: MyUsage,
  validateSearch: (search: Record<string, unknown>): Search => ([7, 90].includes(Number(search.days)) ? { days: Number(search.days) } : {}),
});

// Account menu → My usage (docs/model-gateway-plan.md §9.2): the runs you
// started, by project and model. Only your own usage is ever shown here.
function MyUsage() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : "";
  const days = Route.useSearch().days ?? 30;
  const navigate = useNavigate({ from: Route.fullPath });
  const usage = useQuery({ queryKey: myUsageKey(scope, days), enabled: Boolean(scope), placeholderData: keepPreviousData,
    queryFn: ({ signal }) => getMyUsage(days, signal) });
  const filters = <RangeFilter days={days} onDays={(d) => void navigate({ search: d === 30 ? {} : { days: d }, replace: true })} />;
  const title = "My usage";
  if (usage.isPending) return <DashboardLayout title={title} actions={filters}><StatePanel kind="loading" title="Loading your usage" /></DashboardLayout>;
  if (usage.isError || !usage.data) return <DashboardLayout title={title}><StatePanel kind="error" title="Your usage is unavailable" retry={() => void usage.refetch()} /></DashboardLayout>;
  const u = usage.data;
  if (!u.enabled) return <DashboardLayout title={title}><GatewayOff /></DashboardLayout>;
  const t = u.totals!;
  const fading = usage.isPlaceholderData ? " is-refetching" : "";
  return <DashboardLayout title={title} description={`Model usage from runs you started, ${u.fromDay} to ${u.toDay} (UTC).`} actions={filters}
    tiles={<>
      <StatTile label="My estimated spend" value={usd(t.costUsdMicros)} meta={`${compact(t.requests)} requests`} />
      <StatTile label="My tokens" value={compact(totalTokens(t))} meta={`${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out`} />
      <StatTile label="Cached input" value={compact(t.cacheReadTokens)} meta="Prompt-cache reads" />
      <StatTile label="Projects" value={u.byProject.length} />
    </>}>
    <div className={`dash-wide usage-split${fading}`}>
      <Card title="By project"><SliceTable id="my-usage-projects" label="My usage by project" kind="project" slices={u.byProject} empty="No usage in this period." /></Card>
      <Card title="By model"><SliceTable id="my-usage-models" label="My usage by model" kind="model" slices={u.byModel} empty="No usage in this period." /></Card>
    </div>
    <div className={`dash-wide${fading}`}>
      <Card title="My runs by cost" description="Your runs with metered requests in this period."><SliceTable id="my-usage-runs" label="My runs by cost" kind="run" slices={u.topRuns} empty="None of your runs used the gateway in this period." /></Card>
    </div>
    <div className="dash-wide"><MySubscriptions scope={scope} /></div>
  </DashboardLayout>;
}

// My subscriptions (§6, §9.2): each of my own subscription connections with
// the limit windows its provider reported on my own runs. Only I see these.
export function MySubscriptions({ scope }: { scope: string }) {
  const limits = useQuery({ queryKey: mySubscriptionLimitsKey(scope), enabled: Boolean(scope), refetchInterval: 30_000,
    queryFn: ({ signal }) => listMySubscriptionLimits(signal) });
  const description = "Usage windows your provider reported on your own runs through the gateway. Only you can see these; they are never pooled or shared.";
  if (limits.isPending) return <Card title="My subscriptions" description={description}><p className="card-note">Loading…</p></Card>;
  if (limits.isError) return <Card title="My subscriptions" description={description}><p className="card-note">Your subscriptions are unavailable.</p></Card>;
  const items = limits.data.subscriptions;
  return <Card title="My subscriptions" description={description}>
    {!items.length ? <p className="card-note">You have no subscription connections. Add one in <Link className="text-link" to="/me/connections">My connections</Link>.</p> : null}
    {items.map((sub) => <SubscriptionMeters key={sub.connectionId} sub={sub} personalRoutes={limits.data.personalRoutesEnabled} />)}
  </Card>;
}

function SubscriptionMeters({ sub, personalRoutes }: { sub: SubscriptionLimits; personalRoutes: boolean }) {
  const now = useNow();
  const name = sub.label || (sub.authMethod === "codex_chatgpt" ? "Codex (ChatGPT plan)" : "Claude subscription");
  const off = sub.authMethod === "codex_chatgpt" && !personalRoutes;
  return <section aria-label={name}>
    <p className="card-note"><strong>{name}</strong> · {sub.state === "active" ? "Active" : sentence(sub.state)}
      {off ? " · Codex sign-ins go through the gateway only when an admin turns on personal subscription routes." : ""}</p>
    {sub.windows.length ? <ul className="meter-list">{sub.windows.map((w) => {
      const left = countdown(w.resetsAt, now);
      return <li key={w.name} className="meter-row">
        <span>{windowLabel(w.name, w.windowMinutes)}</span>
        <span className="num">{Math.round(w.usedPct)}% used{left ? ` · resets in ${left}` : ""}</span>
        <meter min={0} max={100} value={Math.min(w.usedPct, 100)} high={80} optimum={0} aria-label={`${windowLabel(w.name, w.windowMinutes)}: ${Math.round(w.usedPct)}% used`} />
        <small>Reported <Timestamp value={w.observedAt} /></small>
      </li>;
    })}</ul> : <p className="card-note">Not reported by provider yet.</p>}
  </section>;
}
