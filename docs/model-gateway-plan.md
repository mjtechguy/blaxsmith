# Blaxsmith model gateway: plan

**Status:** proposed · **Date:** 2026-09-24 · **Owners:** platform, access, web

Related: [`extensions-and-runtimes.md`](extensions-and-runtimes.md),
[`interactive-sessions.md`](interactive-sessions.md), and plan §10.4
(brokered delivery) and §10.10 (usage and billing).

## 1. Goals and non-goals

**Goals, in priority order:**

1. **No raw provider credentials in sandboxes.** Agents get a short-lived,
   run-scoped Blaxsmith token and a gateway base URL. Real keys and OAuth
   tokens never leave the platform. Revocation stops requests at the gateway,
   and a human who takes over a session can't read a provider key.
2. **Rate-limit headroom through legitimate routes.** Pool org API-key and
   cloud routes for the same model family: Anthropic direct, AWS Bedrock and
   Google Vertex for Claude; OpenAI and Azure OpenAI for GPT; OpenCode Zen.
   Pick routes by health and remaining quota, fail over on 429 and 5xx, and
   pace work instead of failing it.
3. **Clear cost and usage visibility** for admins and users: per request,
   run, stage, user, project and org, with limits and reset times wherever
   the provider exposes them, including a user's own subscription.
4. **Soft budgets first.** Threshold alerts, with no blocking in v1. Hard caps
   come later.

**Non-goals, explicitly out of scope:**

- Disguising, "cloaking" or rewriting traffic to look like a different client,
  identity obfuscation, or any anti-detection behaviour. Every upstream
  request is sent as itself, under the account that actually serves it.
- Pooling or rotating OAuth subscription accounts (one user's or several
  users') to combine or exceed per-account limits. Headroom comes only from
  API-key and cloud routes (§4) and pacing (§5).
- Storing prompt or response content **by default**. Metering records counts
  and metadata only. Content capture is available as an opt-in admin setting
  (§15.2).

Personal subscriptions remain supported, as **one route per user connection,
used only for that user's own runs** (§6).

## 2. Architecture

```
sandbox (AX task)                      Blaxsmith platform
┌──────────────────────┐   HTTPS   ┌──────────────────────────────────────┐
│ harness CLI           │──────────▶│ gateway (Deployment: blaxsmith-gw)   │
│  ANTHROPIC_BASE_URL   │  run token│  authn → policy → route pick → proxy │──▶ Anthropic / Bedrock / Vertex
│  OPENAI_BASE_URL      │           │  meter → rate state → alerts         │──▶ OpenAI / Azure OpenAI
│  opencode baseURL     │           │                                      │──▶ OpenCode Zen
└──────────────────────┘           │ writes: gateway_usage_events         │──▶ (user's own subscription route)
                                    └──────────────────────────────────────┘
                                               │  PostgreSQL (shared)
                                               ▼
                                    app: UI, RPCs, rollups, budgets, alerts
```

- **Process:** the same Go binary as the app, run as `blaxsmith gateway`, in
  its own Deployment (and later its own HPA). It shares PostgreSQL and the
  access tables. Keeping it separate isolates streaming load and lets it scale
  on its own.
- **Network:** sandbox egress to provider hosts is **removed** for runs that
  use the gateway. The Gateway/NetworkPolicy allows the sandbox to reach only
  `blaxsmith-gw` (plus Git and whatever the recipe declares). This is the
  enforcement half of goal 1.
- **Protocols:** pass-through proxying of each provider's native API. Nothing
  is translated between API formats in v1:
  - Anthropic Messages, including streaming SSE, tool use, prompt caching
    headers and `anthropic-beta`.
  - OpenAI Responses and Chat Completions, including streaming.
  - OpenCode Zen's OpenAI-compatible API.
  - Bedrock and Vertex routes are served by translating the **transport and
    auth** only (SigV4 or Google auth plus URL mapping), with the same Messages
    body. Both clouds accept Anthropic's Messages format.

## 3. Credential flow

1. At model-lease release (the existing bootstrap connector path), instead of
   sealing the raw provider key, the platform mints a **gateway token**:
   - an opaque random value, stored hashed;
   - bound to organization, project, run, attempt, stage, principal (the run
     initiator), harness, allowed model families, and the lease generation;
   - with an expiry equal to the lease, renewed by the existing lease-renewal
     loop.
2. The worker writes the harness config:

   | Harness | Setting |
   |---|---|
   | Claude Code | `ANTHROPIC_BASE_URL=https://blaxsmith-gw/<route-family>`, `ANTHROPIC_AUTH_TOKEN=<gw token>` (verify `--bare` honours both) |
   | Codex | `OPENAI_BASE_URL` / provider `base_url` in its config, key = gateway token |
   | OpenCode | provider `options.baseURL` + `apiKey` in its scoped `opencode.json` |

3. On every request, the gateway checks the token (hash lookup, cached for at
   most 5 seconds), then the lease, grant and policy through the same code as
   `access.AuthorizeModelInvoke`, then the requested model against the
   project's allowed models. Revoking a lease or grant, or stopping the
   attempt, denies the next request.
4. The gateway picks a route (§4) and injects the **real** credential for it.
   Credentials are decrypted in memory for each route, with a short in-memory
   cache that is cleared on rotation or revocation.

Delivery mode: a new `brokered_gateway` mode next to `native_raw`. Projects
choose per connection. The default for new projects is `brokered_gateway`
once the gateway is GA.

## 4. Routes and pools

- **Route:** a provider connection plus an endpoint kind: `anthropic`,
  `bedrock`, `vertex`, `openai`, `azure_openai`, `opencode_zen`, or
  `personal_subscription`. It has:
  - supported models, mapped from our model ids to the route's ids (e.g.
    Bedrock ARNs and Vertex model names);
  - a weight and a priority;
  - a concurrency cap;
  - a region;
  - state: enabled, draining or disabled.
- **Pool:** a named set of routes serving one model family, e.g. "Claude
  production" = Anthropic key A, Anthropic key B, Bedrock us-east-1 and Vertex
  us-east5. Pools are org-owned and granted to projects like connections,
  through `access.CanUse`.
- **Selection:** filter out unhealthy, cooling-down, over-cap and
  model-unsupported routes, then apply the strategy:
  - `priority-then-headroom` (default): lowest priority number first; among
    equals, the route with the most remaining requests and tokens.
  - `weighted`.
  - `fill-first`.

  Prompt-cache affinity: when a route is healthy, prefer the route last used
  by the same attempt, so provider-side prompt caches stay warm.
- **Failover:** only **before the first streamed byte**. On 429 (with
  retry-after), 408, 5xx, a connect error or a timeout, retry on the next
  route, up to 2 retries. A mid-stream failure is returned to the client as
  the provider's error. Harnesses retry themselves.
- **Circuit breaker for each route:** an error-rate window, open or half-open
  states, and automatic cooldown. The breaker state is visible in the UI.

## 5. Rate-limit awareness and pacing

- **Header parsing per route kind:**
  - Anthropic `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}` and `retry-after`.
  - OpenAI `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`.
  - Azure's equivalents.
  - Bedrock and Vertex throttling errors, with quota taken from configuration,
    since they don't send remaining-quota headers.
- **Route state:** the latest limit, remaining and reset for each metric, kept
  in memory and persisted to `gateway_route_state` about every 5 seconds for
  the UI and for other replicas.
- **Back-pressure to the dispatcher:** before reserving a stage, the
  dispatcher asks the gateway whether the stage's pool has headroom
  (`HeadroomFor(pool, estimated tokens)`). If not, the stage stays queued with
  a visible reason ("waiting for Claude headroom, resets in 42 s") instead of
  failing. The existing idle-timeout logic treats a paced wait as progress.
- **Concurrency caps** per route and per pool stop bursts from hitting a
  cliff.

## 6. Personal subscriptions

- **One route per user connection.** A Codex subscription, or another
  subscription mode allowed by policy, becomes a `personal_subscription`
  route owned by that user. The gateway uses it **only** for runs whose
  initiator owns it, which is the owner-only rule already in place.
  - No pool may contain personal subscription routes.
  - A user cannot attach more than one active subscription per provider to
    the same pool or run.
- **Refresh:** the refresh token stays in the existing central custody
  (`access.OAuthRefresher`). The gateway calls the upstream with a fresh
  access token. Sandboxes no longer receive even the access token.
- **Limits and resets, where the provider exposes them** (each needs
  verifying for the pinned versions):
  - **Codex / ChatGPT:** rate-limit windows (primary and secondary: used
    percent, window minutes, resets at) are reported in the Responses stream
    and by `codex app-server` `account/rateLimits`. Parse them from gateway
    responses.
  - **Claude subscription:** a per-user, owner-only option (§6.1).
  - **Others:** show "Not reported by provider" rather than guessing.
- Shown to the owner in **My usage** (§9.2). Admins see only aggregate usage
  counts for personal routes, never the account identity or tokens.

### 6.1 Claude subscription for its owner

**Status:** built (branch `claude-sub`), native_raw delivery. Gateway delivery
for this auth method is a follow-up: the gateway would have to inject
`Authorization: Bearer` plus the OAuth beta header instead of `x-api-key`.

**Scope: per user, owner-only.** It's the self-hosted equivalent of putting
your own `claude setup-token` in your own CI secrets. It's not a platform-level
Claude account, and never a pool.

- **Connect:** under My connections → + Subscription → Claude, the user pastes
  the output of `claude setup-token` (a long-lived token, prefix
  `sk-ant-oat…`, with no refresh token).
  - Validated for shape only, stored encrypted like other secrets, and never
    returned by any API.
  - Revocable at any time.
- **Use:** only for runs **the owner starts**, enforced in
  `access.deliveryAllowed` with the same owner-only rule as Codex
  subscriptions (§6):
  - never granted to a workload, a role, a project or another user;
  - never in a pool;
  - admins see that the connection exists, but not its value.
- **Delivery:**
  - it travels on the existing long-lived API-key lease path, so the AX runner
    needs no change;
  - inside the sandbox, the worker recognises the `sk-ant-oat` prefix and
    delivers the token as `CLAUDE_CODE_OAUTH_TOKEN` (environment only, never
    on disk or in argv);
  - it's redacted everywhere, like the other credentials.

  With the gateway (§3), the token stays on the platform and the gateway calls
  upstream with it for that user's runs only.
- **Isolation:** `--bare` ignores `CLAUDE_CODE_OAUTH_TOKEN`, so these stages
  run `claude -p` in non-bare mode with the flags and environment the
  credential-free probe verified (`docs/claude-subscription-probe.json`):
  - `--setting-sources project`;
  - `--strict-mcp-config --mcp-config '{"mcpServers":{}}'`;
  - `disableAllHooks` in `--settings`;
  - a fresh `CLAUDE_CONFIG_DIR`;
  - `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1` and
    `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`.

  The worker refuses to run if `.claude`, `CLAUDE.md`, `CLAUDE.local.md` or
  `.mcp.json` exists above the checkout. The probe showed ancestor project
  skills load in this mode. API-key Claude stages stay on `--bare`, unchanged.
- **Limits:** all of the owner's runs share that one account's usage limits,
  the same as when they work interactively. Pacing (§5) spreads the owner's
  concurrent stages rather than multiplying their limits.
- **Admin control:** an org setting, "Allow members to use their own Claude
  subscription for their own runs", off by default. Enabling it shows
  Anthropic's terms link, and the change is audited.
- **Tests to include:** prefix detection, owner-only binding (another user's,
  a workload's or a role's run is denied), argv and environment rewriting,
  refusal on ancestor config, redaction, and a mock-provider run proving the
  token is sent as an OAuth bearer and not as `x-api-key`.

## 7. Metering and pricing

- **One usage event per completed or failed upstream request** (table
  `gateway_usage_events`, partitioned by month), holding:
  - org, project, run, attempt, stage, principal, harness;
  - pool, route and route kind;
  - requested model and served model;
  - status, HTTP code, retry count;
  - start time, time to first token, total duration;
  - input, output, cache-read and cache-write tokens, and reasoning tokens
    where reported;
  - estimated cost in USD micros, and the id of the price-table version used.

  Counts come from the provider's final usage block, streamed or not. No
  prompt or response text is stored.
- **Pricing:** a versioned price table per model and route kind, with separate
  input, output, cache-read and cache-write rates. It is bundled in the model
  manifest and admins can override it with contracted rates. Cost is always
  labelled "estimated".
- **Rollups:** a background job maintains `gateway_usage_daily` by (org,
  project, principal, pool, route, model, day), plus per-run totals on the run
  row, so the UI reads quickly without scanning raw events.
- **Retention:** raw events 90 days (configurable), rollups kept, following
  plan §11.

## 8. Budgets and soft alerts (v1)

- **Budget:** scope (org, project or user), period (month, or a custom window
  with its own reset day), an amount in USD or tokens, and thresholds
  (default 50/80/100%), plus optional per-model or per-pool filters.
- **Evaluation:** on each rollup tick, and immediately when a request pushes
  spend past a threshold. Crossing a threshold creates an **alert**:
  - an inbox item for the budget owner and admins;
  - an entry in the Admin alert feed;
  - an audit event;
  - optionally a webhook later.

  Each threshold fires once per period, with a snooze option.
- **No blocking in v1.** Hard caps ("stop dispatch", "limit to cheaper
  models") are a v2 policy switch per budget.
- **Forecast:** a linear projection for the current period ("at this rate:
  $412 by Oct 31"), shown next to each budget.

### 8.1 As built (G3, migration 0170)

- **Budgets** (`gateway_budgets`): scope organization, project or user; one
  active budget per scope target; a monthly amount in estimated USD over the
  **UTC calendar month**; thresholds 1–6 distinct percents (1–200), default
  50/80/100. Owners and admins create, edit (name, amount, thresholds, with
  optimistic concurrency) and archive them; every change is audited. Custom
  windows, token amounts and per-model or per-pool filters are deferred.
- **Switch:** "Budgets & alerts" (§15.1) lives in `gateway_budget_settings`,
  off by default, toggled on Admin → Budgets and audited. Budgets show
  progress either way; alerts fire only while both it and the gateway master
  switch are on.
- **Evaluation:** `workflow.EvaluateBudgets` runs in the gateway maintenance
  loop right after each rollup tick (about once a minute), comparing
  month-to-date spend from `gateway_usage_daily` against each budget. Only the
  highest newly crossed threshold fires (40% → 90% alerts once, at 80%).
  On-request evaluation is deferred: rollups already lag by at most a minute.
- **Exactly once:** `gateway_alerts` is unique on (budget, period, threshold).
  The alert row and its `gateway.budget.threshold_crossed` audit event
  (actor `system`) are written in one transaction, so concurrent rollup ticks
  from several replicas produce one alert and one audit event; a losing
  insert writes nothing.
- **Delivery:** the alert appears in the inbox (kind `budget_alert`) for its
  recipients (organization owners and admins, the budget's user, and the
  project's administrators) until acknowledged, which closes it for everyone,
  or while snoozed (1 hour to 30 days). Acknowledge and snooze are audited.
  Webhooks and email are deferred.
- **Forecast:** month-to-date spend × days in month ÷ days elapsed, with at
  least one elapsed day.
- **Pages:** Admin → Budgets (table with progress, forecast and fired
  thresholds, the switch, add/edit/archive), Admin → Alerts (feed with
  acknowledge and snooze), and Project → Usage (§9.3: spend against the
  project budget, spend by model over the month, spend by stage, model and
  user, top runs, and the project's alerts). Spend by user is shown only to
  project administrators. Pool headroom on Project → Usage waits for G2
  pools.

## 9. UI

It is built on the release-3 templates (DashboardLayout, DetailLayout,
SettingsLayout, CollectionTable), in light and charcoal dark. Charts follow
the `dataviz` guidance (one palette system, accessible, consistent in both
themes). Every number links to the table behind it.

### 9.1 Admin → Usage & Gateway (new sidebar group items)

- **Overview (dashboard):**
  - stat tiles: spend this period vs budget, tokens, requests, error rate,
    429 rate, median time to first token;
  - a spend-over-time chart by pool, model or project, toggleable;
  - top projects, users and runs by cost;
  - an active-alerts panel.
- **Routes & pools:**
  - a CollectionTable of routes: kind, pool, models, health (breaker state),
    requests remaining and tokens remaining with **reset countdowns**, current
    concurrency against the cap, error rate over the last 15 minutes;
  - an expandable row showing the per-metric limits and the last 429 events;
  - a pool detail page with tabs: Routes · Traffic · Failovers · Settings
    (strategy, caps, affinity);
  - actions: drain, disable, re-enable.
- **Usage explorer:**
  - filterable by date range, org, project, user, model, pool, route, harness
    and status;
  - group-by and a stacked chart, with the table below it;
  - export to CSV;
  - drill down to the runs behind any cell.
- **Budgets:** a CollectionTable plus a create flow (scope → period → amount →
  thresholds → notify). Each row shows its progress bar and forecast.
- **Alerts:** a feed with acknowledge and snooze.
- **Pricing:** the price table, with overrides and effective dates.

### 9.2 User (account menu → My usage)

- **My usage:**
  - my spend and tokens this period, broken down by project and model;
  - my runs by cost;
  - any budgets that apply to me, with progress.
- **My subscriptions:** for each personal subscription connection:
  - its status;
  - rate-limit windows as meters (used % against the window), each with a
    **reset time** and countdown, where the provider reports them;
  - recent 429s;
  - "Not reported by provider" where it doesn't.
- **Notifications:** budget alerts that concern me, from the inbox.

### 9.3 Project → Usage (new project nav item)

Project spend vs its budget, spend by stage kind, model and user, a list of the
most expensive runs, and pool headroom for the pools this project uses (for
example "Claude production: 72% of tokens remaining, resets in 38 s").

### 9.4 Run page → Cost tab, plus stage chips

- Run totals.
- Per-stage cost, tokens and cache-hit ratio.
- Each request, with its route, retries and failovers.
- A "paced" timeline showing where a stage waited for headroom.
- Each stage row shows a small cost chip.

**Privacy (§6):** when a run's model binding used a personal (user-owned)
connection, or a `personal_subscription` route served it, per-request rows
are shown only to that connection's owner and to organization owners and
admins. Other members see the run totals and per-stage aggregates only
(`GetRunCostResponse.requests_restricted`).

## 10. Data model (migrations 0100+)

| Table | Contents |
|---|---|
| `gateway_tokens` | hashed token, org, project, run, attempt, stage, principal, harness, lease id and generation, allowed families, expiry, revoked_at |
| `gateway_routes` | org, connection, kind, region, model map, weight, priority, concurrency cap, state |
| `gateway_pools`, `gateway_pool_routes` | pool definitions and membership, with a check that forbids `personal_subscription` routes |
| `gateway_route_state` | route, metric, limit, remaining, reset_at, breaker state, updated_at |
| `gateway_usage_events` | §7 fields, partitioned monthly |
| `gateway_usage_daily` | rollups |
| `gateway_price_versions`, `gateway_prices` | pricing, including overrides |
| `gateway_budgets`, `gateway_budget_thresholds`, `gateway_alerts` | §8 |
| `gateway_subscription_limits` | personal route, window name, used_pct, window_minutes, resets_at, observed_at |

Every table is tenant-keyed, following existing conventions. Grants use
`access_resource_grants` with resource kind `gateway_pool`.

## 11. APIs

- **`GatewayAdminService`** (owners and admins):
  - routes: list, create, update, drain, disable;
  - pools: list, create, update, grant;
  - `GetRouteState`;
  - `QueryUsage` (group-by, filters, paged);
  - `ExportUsage`;
  - budgets: create, update, list;
  - alerts: list, acknowledge, snooze;
  - pricing: list, override.
- **`UsageService`** (any member, scoped to self or project by role):
  - `GetMyUsage`, `ListMySubscriptionLimits`;
  - `GetProjectUsage` (project members);
  - `GetRunCost` (members who can view the run).
- **Internal:**
  - `HeadroomFor(pool, estimate)`, used by the dispatcher;
  - the token mint and revoke hooks used by the lease code.

Mutations are CSRF-protected and audited; reads are tenant- and role-filtered.

## 12. Security

- Tokens are run-scoped, stored hashed, short-lived, and revoked with the
  lease. They grant **no** access to anything but model calls for their
  families.
- The gateway strips inbound `authorization`, `x-api-key` and cloud-auth
  headers from the client and injects only the route credential.
- Response headers that could leak account identity (organization ids,
  account emails) are removed before they reach the sandbox.
- Logs never include tokens, keys or bodies. Request IDs are kept for
  support.
- The NetworkPolicy removes direct provider egress for gateway-mode runs.
- Personal routes are owner-only and are never pooled, shared or rotated.
  API-key pools can never contain subscription accounts. There is no cloaking
  or identity rewriting anywhere.
- Load: per-token and per-org request rate limits at the gateway protect
  against a runaway agent.

## 13. Harness compatibility: verify before GA

| Check | Claude Code | Codex | OpenCode |
|---|---|---|---|
| Custom base URL plus bearer honoured in our isolation mode (`--bare` / ignore-user-config / scoped config) | verify | verify | verify |
| Streaming passes through unchanged (SSE framing, keep-alives) | test | test | test |
| Tool use and function calling | test | test | test |
| Prompt-caching headers preserved, and cache-hit tokens reported | test | n/a | test |
| Final usage block parseable for metering | test | test | test |
| 429 with retry-after handled by the CLI's own retry | test | test | test |

A live probe per harness through the gateway, credential-free with a mock
upstream first and then real keys, is added to the proof set like the other
probes in `docs/`.

## 14. Rollout

1. **G1, brokered pass-through** (goal 1):
   - gateway Deployment, tokens, auth, one route per connection, no pools;
   - metering to events and rollups;
   - the run cost tab and My usage;
   - the NetworkPolicy change;
   - harness probes.

   Acceptance: a run on the preview works end to end with no provider key in
   the sandbox (checked by scanning the sandbox environment and files), and
   the cost appears on the run.
2. **G2, pools and headroom:** routes, pools, selection, failover, the circuit
   breaker, rate-limit parsing, route state, dispatcher back-pressure, and
   Admin Routes & pools. Acceptance: with two routes, a forced 429 on one
   fails over before the first byte, and a saturated pool queues a stage with
   a visible reset countdown.
3. **G3, budgets and alerts:** budgets, thresholds, alerts, forecast, the
   Admin Budgets and Alerts pages, and Project → Usage. Acceptance: crossing
   80% creates exactly one inbox alert and one audit event. **Built** (§8.1);
   the acceptance test runs eight concurrent evaluations
   (`TestBudgetsAlertsExactlyOnce`).
4. **G4, subscriptions and Bedrock/Vertex:** personal subscription routes
   with limit and reset meters (Codex first), and the Bedrock and Vertex
   transport adapters.
5. **Later:** hard caps, webhooks, API-format translation between providers
   (only if needed), and per-team chargeback reports.

## 15. Optional system: feature flags and settings

The gateway is **off by default** and fully optional. Blaxsmith keeps working
exactly as today (`native_raw` delivery) when it's disabled.

### 15.1 Admin → Settings → Model gateway (owners and admins)

| Setting | Default | Effect |
|---|---|---|
| **Enable model gateway** | off | The master switch for the org. When off, the gateway rejects this org's tokens, no gateway tokens are minted, and all gateway UI apart from this page is hidden |
| **Default delivery mode for new projects** | `native_raw` | `native_raw` or `brokered_gateway` |
| **Allow projects to choose delivery mode** | on | Project admins can switch a project between modes (Project → Settings → Model access). When off, the org default is enforced |
| **Remove direct provider egress for gateway runs** | on | Applies the NetworkPolicy/Gateway restriction in §2. It can be turned off for debugging, and shows a warning when off |
| **Pools & failover** | off | Enables §4 pools and selection. When off, one route per connection |
| **Rate-aware pacing** | off | Enables §5 dispatcher back-pressure |
| **Budgets & alerts** | off | Enables §8 |
| **Personal subscription routes** | off | Enables §6 for this org, still owner-only |
| **Allow members' own Claude subscriptions** | off | Enables §6.1: each member may use their own `claude setup-token`, only for runs they start. Lives in Admin → Settings → Connections; it is independent of the gateway master switch |
| **Content capture** | off | §15.2 |

- **Rollout:** each flag is evaluated per request and per dispatch. Changes
  are audited (who, when, old → new).
- **Turning the master switch off** drains cleanly: in-flight streams finish,
  new requests from gateway-mode runs fail with a clear "gateway disabled by
  your admin" error, and those projects fall back to `native_raw` on their
  next run if the project allows it.
- **Installation level:** an operator setting in the Helm values
  (`gateway.enabled`) controls whether the gateway Deployment exists at all.
  The org switch can only be turned on when it does.

### 15.2 Optional content capture

- **Off by default.** When on (per org, optionally narrowed to projects), the
  gateway stores the request and response bodies of model calls alongside
  each usage event.
- **Redaction** runs before anything is stored: known secret patterns, the
  run's leased credentials, and configured regexes.
- **Encryption and retention:** stored with the same access-key envelope as
  secrets, with a retention period (default 14 days, max 90) and a purge job.
- **Viewing:** needs a separate `view_model_content` permission (owners and
  admins by default). Every view is audited. Content shows in the run Cost tab
  and the Usage explorer's request detail.
- **Transparency:** a banner on affected projects' run pages: "Model traffic
  for this project is recorded for N days."

### 15.3 Never offered, flag or no flag

- Pooling or rotating subscription accounts.
- Client impersonation, "cloaking", header or identity rewriting,
  fingerprint randomisation, or any detection-evasion behaviour.

## 16. Open questions

1. Which cloud routes can we actually use? Do we have Bedrock or Vertex
   accounts for Claude, and Azure OpenAI? These are the biggest legitimate
   headroom win.
2. ~~Is the budget currency USD estimates only, or do we also need token
   budgets per team?~~ **Decided (G3): USD estimates only**, from list or
   contracted rates, over the UTC calendar month. Token budgets and team
   scopes are not built.
3. Retention for raw usage events: is 90 days acceptable?
4. Does any org need usage reports by cost centre or team (a tags model on
   projects)?
5. Should the gateway be the default delivery mode immediately after G1, or
   opt-in per project until G2?
