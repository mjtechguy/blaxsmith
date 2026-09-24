# Coding-plan subscription logins

Checked on 2026-09-24 against provider docs and source. Items marked
*unofficial* come from public observation, not provider documentation.

## Policy

| Mode | Verdict | Blaxsmith behavior |
|---|---|---|
| Codex with a ChatGPT-plan sign-in | Allowed with conditions: the account owner's own use, on trusted private infrastructure, with no concurrent sharing of one `auth.json`. OpenAI still recommends API keys for automation. | Enabled as `oauth_access`, owner-only. |
| Claude Code with a Pro/Max subscription (`claude setup-token` or `/login`) | Disallowed for Blaxsmith as a platform. Anthropic: "developers may not collect, store, or intermediate Claude.ai credentials or session tokens" and may not "route requests through Free, Pro, or Max plan credentials on behalf of their users". | Disabled by policy. `access.ErrClaudeSubscriptionDisabled` returns the reason to the API and UI. Use an Anthropic API key. |

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

## Not done

- **Guest.** The pinned AX runner patch accepts only `model_api_key`. A `model_codex_auth` envelope is therefore rejected in the guest, which fails closed. Writing `auth.json` into the worker's temporary HOME is not implemented yet.
- **Dispatch.** Dispatch always binds the `workload` grantee, so personal grants are denied until a run carries its initiating user as grantee.
- **Device code.** Blaxsmith does not start device-code flows. Users paste `auth.json`.
- **Reconnect and cleanup.** Reconnect creates a new connection. `SecretStore.Rotate` on a refreshed connection fails on a version conflict, which fails closed. Superseded secret versions are not pruned.
- **Live endpoint.** Nothing has been tested against the live `auth.openai.com` endpoint or a real Codex CLI.
