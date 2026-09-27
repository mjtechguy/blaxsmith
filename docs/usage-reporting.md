# Reported usage

`GetRunUsage` and `GetGoalUsage` return exact decimal token subtotals, an estimated cost subtotal in micro-USD, and report coverage. The browser shows them under **Reported usage**. Both machine transports expose the same reads with `project.read` and enforce the credential's project. Delivery exports read usage in the same database snapshot as acceptance and evidence.

The counters include retained reports from failed, retried and cancelled attempts. Missing reports are unknown, not zero. `reports=0` means no token measurement is available; `cost_reports=0` means cost is unknown, even when the subtotal string is `"0"`. An explicit zero reported by a harness remains distinguishable from missing data. Aggregate sums use database numeric arithmetic and decimal strings to avoid integer overflow or JavaScript precision loss.

## Collection and attribution

The pinned worker normalizes only native terminal usage events:

| Harness | Report identity | Input total | Output total | Cost |
| --- | --- | --- | --- | --- |
| Codex | One autonomous session per attempt | `input_tokens`, already includes cached input | `output_tokens`, already includes reasoning | Unknown |
| Claude Code | One autonomous session per attempt | Input + cache read + cache creation | Result output tokens | `total_cost_usd`, if valid |
| OpenCode 2.0.14 | Stable step ID, hashed | Noncached input + cache read + cache write | Visible output + reasoning | Step cost, if valid |

The attempt starts a fresh autonomous session. Human takeover uses a TUI and is not measured by this collector. Assistant-message usage is not added to terminal summaries. Codex and Claude session replays cannot add a second session total; OpenCode step replays cannot count the same step twice. A changed replay is refused. The CLI's reported error result can still contain consumption.

The worker publishes reports through the existing durable guest watch log. The coordinator supplies the attempt identity; the guest cannot choose another run, attempt or organization. Persistence checks the frozen harness and attempt fence, retains immutable rows, and advances the watch cursor in the same transaction. Late reports may be retained after an attempt stops. A bounded, three-second final read after a signed command exit collects the unconsumed usage tail before disposal. Failed collection does not delay termination indefinitely or turn missing coverage into zero. Cancellation prioritizes termination; only reports already collected are guaranteed to remain.

Each report is bounded to one trillion tokens per counter / micro-USD, with at most 10,000 reports per attempt. Out-of-range, fractional token counts and malformed/missing token fields are refused. Cost is parsed with bounded decimal arithmetic and rounded up to the nearest micro-USD; invalid/missing cost remains unknown.

## Confidence and remaining accounting work

All values are **unverified harness reports**. The sandbox can forge or omit them. Even reports from every attempt do not establish complete measurement: provider overhead, lost events, malformed output, crashes and TUI work may be absent. No quota percentage, invoice charge, price-table version or billing reconciliation is inferred. Harness cost uses the harness's own pricing information, which may be missing, stale or unrelated to subscription charges.

These records do not enforce cash/token ceilings. Goal run/attempt/deadline allowances remain separate admission controls. Reliable hard spending limits require provider-enforced limits or a qualified metered route, reservations for concurrent work, and settlement that handles missing usage. Native raw credentials do not provide that guarantee.

Sources reviewed 2026-09-26: [Codex pinned event definitions](https://github.com/openai/codex/blob/rust-v0.156.1/sdk/typescript/src/events.ts), [Claude usage and cost semantics](https://platform.claude.com/docs/en/agent-sdk/cost-tracking), [OpenCode 2.0.14 token normalization](https://github.com/anomalyco/opencode/blob/v2.0.14/packages/core/src/session/usage.ts). CLI/provider live qualification remains separate from the local fixtures.
