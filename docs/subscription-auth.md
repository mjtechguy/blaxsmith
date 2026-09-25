# Coding-plan subscription logins

Checked on 2026-09-24 against provider docs and source. Items marked
*unofficial* come from public observation, not provider documentation.

## Policy

| Mode | Verdict | Blaxsmith behavior |
|---|---|---|
| Codex with a ChatGPT-plan sign-in | Allowed with conditions: the account owner's own use, on trusted private infrastructure, with no concurrent sharing of one `auth.json`. OpenAI still recommends API keys for automation. | Enabled as `oauth_access`, owner-only. |
| Claude Code with a member's own `claude setup-token` | Anthropic: "developers may not collect, store, or intermediate Claude.ai credentials or session tokens" and may not "route requests through Free, Pro, or Max plan credentials on behalf of their users". Blaxsmith therefore offers this only as a self-hosted, per-member choice: an org owner/admin must opt in after reviewing the terms, and each token is used solely for its owner's own runs (the equivalent of the owner's own CI secret), never pooled or shared. | `claude_setup_token`, owner-only (`access.deliveryAllowed`), off by default behind Admin → Settings → Connections, rechecked at every release and on every gateway request. In a `native_raw` project the worker runs Claude Code non-bare with the probe-verified isolation (`internal/tooladapter/claude_subscription.go`). In a `brokered_gateway` project the token never enters the sandbox: the gateway sends it upstream as `Authorization: Bearer` with the `oauth-2025-04-20` beta for the owner's own run, and Claude Code runs `--bare` with the run's gateway token. `/login` sessions are not supported. |

Sources:
[Claude Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance),
[Anthropic consumer terms](https://www.anthropic.com/legal/consumer-terms),
[Codex auth](https://developers.openai.com/codex/auth),
[Codex CI/CD auth](https://learn.chatgpt.com/docs/auth/ci-cd-auth) ("Do not share the same file across concurrent jobs or multiple machines"),
[OpenAI terms of use](https://openai.com/policies/row-terms-of-use/) ("You may not share your account credentials or make your account available to anyone else").

Anthropic does allow an end user to sign in to the unmodified `claude` binary
themselves, including on a hosted platform. Blaxsmith storing that token is a
separate thing and is not allowed. Separately, the Claude Code adapter runs
`--bare`, and bare mode "does not read `CLAUDE_CODE_OAUTH_TOKEN`"
([authentication](https://code.claude.com/docs/en/authentication)).

## Native credential facts

**Codex** ([`codex-rs/login`](https://github.com/openai/codex/tree/main/codex-rs/login))
- `~/.codex/auth.json` (or `$CODEX_HOME/auth.json`) holds `{"OPENAI_API_KEY", "tokens":{"id_token","access_token","refresh_token","account_id"}, "last_refresh"}`.
- `refresh_token` is a required string: a missing key fails parsing, but `""` parses.
- The CLI refreshes itself when the access JWT `exp` is within 5 minutes, when `last_refresh` is more than 8 days old and `exp` can't be read, or after a 401. It refreshes by POSTing to `https://auth.openai.com/oauth/token` with client `app_EMoamEEZ73f0CkXaXp7hrann`.
- Refresh tokens are single-use. Reusing one returns `refresh_token_reused`.

**Claude Code** ([authentication](https://code.claude.com/docs/en/authentication))
- `claude setup-token` prints a one-year token for `CLAUDE_CODE_OAUTH_TOKEN`. It has no refresh token and is intended for CI.
- `/login` stores `~/.claude/.credentials.json` with mode 0600. The CLI refreshes that login itself.
- *Unofficial:* the file shape is `{"claudeAiOauth":{accessToken, refreshToken, expiresAt(ms), scopes, subscriptionType}}`. The token endpoint and client ID are not documented.

## Custody design (Codex)

1. **Connect.** The user runs `codex login --device-auth` on their own machine and pastes `auth.json`. `access.ParseCodexAuth` rejects anything that is not a ChatGPT sign-in with a refresh token and JWT access and id tokens. The whole token bundle is encrypted into `access_secret_versions` under a user-owned connection (`auth_method=codex_chatgpt`). The only grant is `oauth_access` to that same user.
2. **Authorize.** `AuthorizeModelInvoke` and `PreflightModelInvoke` accept `oauth_access` only when:
   - the connection is user-owned,
   - the grantee is `user` and equals the owner,
   - the provider is `openai`.

   A `codex_chatgpt` connection can never be delivered through `native_raw`.
3. **Refresh.** `OAuthRefresher` is the only caller of the token endpoint. It works as follows:
   - It takes `SELECT … FOR UPDATE` on the connection's `access_oauth_sessions` row, in its own transaction.
   - It refreshes only if the cached access token expires within `until + 10m`.
   - It commits the rotated version before any access token is returned.
   - It never locks `access_connections`, so it cannot deadlock with release transactions that hold `FOR SHARE` there.
   - It handles failures as follows:

     | Result | Behavior |
     |---|---|
     | 429 | Returned as a retryable error. |
     | 400/401 | Sets `reconnect_reason=refresh_rejected`. |
     | Transport error, 5xx, or bad body | Sets `refresh_outcome_unknown`. Blaxsmith never retries a refresh token that may already be spent. |
4. **Deliver.** The bootstrap envelope carries `kind=model_codex_auth` with an `auth.json` holding:
   - the access token and id token,
   - `refresh_token: ""`,
   - `last_refresh: now`.

   The CLI therefore cannot refresh, and cannot rotate the login away from sibling sandboxes. The token outlives the lease by more than Codex's 5-minute window. **Flag:** an empty string is the workaround because the field is required. If the CLI does try to refresh, that attempt fails.

## Renewal seam

```go
d, err := refresher.RenewOAuthDelivery(ctx, tx, access.OAuthLease{
    Invoke: invoke, LeaseID: leaseID, Until: newLeaseExpiry})
// push d.File to <tool HOME>/d.FileName (".codex/auth.json"), then clear it
```

In the caller's transaction, `RenewOAuthDelivery`:
- re-authorizes the frozen binding (grant, policy, and owner-only rule),
- requires the delivered, unrevoked, unexpired lease.

Refreshing, if needed, commits in the refresher's own transaction first. The loop must keep the lease expiry at or before `d.ExpiresAt - 5m`. `RenewOAuthDelivery` is a method on `*access.OAuthRefresher`, because it needs the pool, keys and HTTP client.

The dispatch leader's renewal loop registers this for `oauth_access`
(`cmd/blaxsmith/lease_renewal.go`). `OAuthRefresher.RenewLease` rebuilds the
binding from the lease, calls `RenewOAuthDelivery`, and caps the lease at the
token's expiry minus 5 minutes in the same transaction. The hook then reads the
path the worker recorded in `/tmp/blaxsmith/codex-auth-path`. It must match
`/tmp/blaxsmith-tool-<n>/.codex/auth.json`. The hook rewrites that file in
place with guest `WriteFile`, which truncates the existing file, so the worker
keeps ownership and mode 0600. Only then does it announce the new expiry.

## Guest delivery

- **Runner.** `integrations/ax/codex-auth-credential.patch` accepts `model_codex_auth` in the model phase only for the Codex harness and provider `openai`. The `auth.json` must be at most 16 KiB, with an access token and a present, empty `refresh_token`. Every other kind, provider, or harness combination fails closed. The runner writes the same 0600 credential file it uses for API keys, as `codex_auth_json`.
- **Worker.** `tooladapter` writes it to `$CODEX_HOME/auth.json` (0600) under the attempt's temporary home and sets `CODEX_HOME`. It never sets `OPENAI_API_KEY` for this mode. The resume pane skips `codex login --with-api-key`. The pane and activity redactors hide the access and id tokens, including renewed ones.
- **Dispatch.** Runs record `initiator_principal_id` at launch. A personal (user-owned) grant is bound only when the run's initiator owns that connection and is its grantee; otherwise the project's workload selection applies (`TestDispatchBatchPostgres` covers a Codex subscription).
- **Egress.** Codex with a ChatGPT sign-in talks to `chatgpt.com`, not `api.openai.com`. The `exact` Gateway mode checks only `api.openai.com` for provider `openai`, so subscription runs need `open-dev` egress until `providerHost` accounts for the delivery mode.

## Through the model gateway (personal routes)

Never pooled, rotated, failed over or disguised: each subscription is one
route used only for its owner's own runs (docs/model-gateway-plan.md §6).

- **Claude setup-token:** in a `brokered_gateway` project the gateway injects
  the token as an OAuth bearer (above). Its usage windows
  (`anthropic-ratelimit-unified-*`, unofficial) are recorded for the owner.
- **Codex sign-in:** with Admin → Settings → Model gateway → *Personal
  subscription routes* on, a `brokered_gateway` attempt gets a gateway token
  instead of `auth.json`. The gateway takes a fresh access token from
  `OAuthRefresher` (still the only caller of the token endpoint) and sends
  the request to `https://chatgpt.com/backend-api/codex` with the account's
  `chatgpt-account-id`, exactly where Codex itself would. Lease renewal
  treats these leases as gateway leases (nothing to push to the guest). The
  `x-codex-{primary,secondary}-*` headers feed the owner's meters in My
  usage. With the switch off, a Codex sign-in keeps its native delivery.
  Not yet tried against the live backend.

## Not done
- **Device code.** The connections hub runs the Codex device-code sign-in server-side (`access.CodexDevice`, from `codex-rs/login/src/device_code_auth.rs`): `POST /api/accounts/deviceauth/usercode`, the user approves at `/codex/device`, `POST /api/accounts/deviceauth/token` polls, and `/oauth/token` exchanges the code once. Pasting `auth.json` stays as the Advanced fallback. Pending sign-ins are encrypted, session-bound rows in `access_pending_sign_ins`, so any replica can finish one. Not yet tried against the live endpoint.
- **Reconnect and cleanup.** Reconnect creates a new connection. `SecretStore.Rotate` on a refreshed connection fails on a version conflict, which fails closed. Superseded secret versions are not pruned.
- **Live endpoint.** Nothing has been tested against the live `auth.openai.com` endpoint or a real Codex CLI.
