# MCP OAuth authorization

Blaxsmith supports authorization-code OAuth for pre-registered public clients connecting to remote `/mcp`. It implements resource binding, S256 PKCE, explicit browser consent and issuer identification using the [MCP 2026-07-28 authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization). The initial integration targets server-side MCP clients; browser cross-origin token/MCP calls, refresh tokens, dynamic registration and Client ID Metadata Documents are not implemented.

## Configure clients

Create an operator-owned JSON registry and set `BLAXSMITH_MCP_OAUTH_CLIENTS_FILE` to its absolute path before starting Blaxsmith:

```json
[
  {
    "client_id": "my-factory-client",
    "client_name": "My factory client",
    "redirect_uris": ["https://client.example.com/oauth/callback"]
  }
]
```

The registry contains public client metadata, not credentials. Maximums: 32 KiB, 32 clients, 8 redirects per client. Unknown fields, duplicate IDs and invalid redirects fail startup. HTTPS redirects match exactly. Native callbacks may use a loopback IP HTTP URL with a specified port; the same host/path/query can use another port. `localhost`, non-loopback HTTP, fragments, userinfo and reserved authorization-response parameters are rejected. Registrations are loaded at startup and require restart to change. Removing a client blocks new authorization/exchange; revoke existing credentials separately in Agent access.

Omitting the registry disables OAuth discovery and issuance. It does not revoke existing credentials or disable provisioned-token MCP. No remote metadata URL is fetched, and no client-provided redirect is trusted without its registration.

## Discovery and flow

- Protected resource metadata: `/.well-known/oauth-protected-resource/mcp`, resource identifier `<origin>/mcp`.
- Authorization server metadata: `/.well-known/oauth-authorization-server`, issuer `<origin>`.
- Authorization endpoint: `/oauth/authorize`.
- Token endpoint: `/oauth/token`.

Unauthenticated `/mcp` responses include the protected-resource metadata URL and the minimum `project.read` scope. The authorization request includes `response_type=code`, registered `client_id`, registered `redirect_uri`, exact `resource`, S256 `code_challenge`, `code_challenge_method=S256`, and optional `state`. Scope defaults to `project.read`. Additional available scopes are `goal.write`, `run.launch`, `run.control` and `review.decide`; `project.read` remains necessary for discovery and observation.

The browser signs in through normal Blaxsmith authentication, selects one project, and sees the client ID, callback, resource and requested permissions. Only read access is selected initially. The user may grant a subset of requested permissions or deny the request. The consent mutation requires Origin and CSRF protections. Viewers can grant only read access. Selected scopes cannot exceed the client's request or bypass domain permissions.

Callbacks include the original `state`, when supplied, and exact `iss`. On approval they include a short-lived code; denial returns `error=access_denied`. Clients must verify state and issuer before exchange. Codes expire after two minutes, are stored only as hashes, and are bound to the consenting browser session, principal, project, client, callback, resource, scopes and PKCE challenge.

POST a form to `/oauth/token` with `grant_type=authorization_code`, `client_id`, `code`, `code_verifier` and exact `resource`. If `redirect_uri` is sent, it must match the authorization request. The endpoint accepts public clients (`token_endpoint_auth_methods_supported: ["none"]`); it rejects browser cookies, Origin headers and HTTP Authorization credentials. It applies the existing database-backed limiter, with 60 exchanges per source IP per minute, ignoring forwarded client headers.

Code consumption and token issuance commit atomically. Current user membership, role, login/MFA policy and the consenting browser session are rechecked before issuance. Concurrent exchanges cannot issue independent credentials from one code. A valid repeat of a consumed code revokes the previously issued credential and returns `invalid_grant`; restart authorization after a lost exchange response.

## Credential lifecycle

Access tokens expire after one hour and are restricted to `<origin>/mcp`. They cannot be used directly with `/machine`, browser APIs or the stdio bridge. Every MCP request still uses the same project checks, scopes, audit path and transactional session fences as provisioned credentials. OAuth adds no provider account access or new review authority.

The credential appears under **Project → Settings → Agent access**, labeled `MCP: <client name>` and marked **MCP only**. Revocation, user deactivation, membership changes and applicable login/MFA policy changes invalidate it. Revoking access does not stop work already running; use the platform's goal/run controls and observe confirmed termination separately. Tokens remain hashed in storage. Code hashes are retained for reuse detection and pruned after a day on subsequent consent by the same principal.

There is no refresh grant: after expiry, the client obtains fresh browser consent. Provisioned credentials remain the supported route for unattended services. The operator must configure a trusted network boundary before deployment behind a shared proxy; the application uses the direct peer address for exchange limits.

## Qualification

The local full E2E suite exercises real HTTPS/PostgreSQL, metadata discovery, registered redirect checks, scope reduction/expansion rejection, CSRF, PKCE, resource/client/callback binding, expiry, code reuse, rate limiting, official-SDK MCP reads and credential revocation. Browser E2E exercises denial, explicit project/scope selection, authorization callback, token exchange, endpoint isolation and Agent access revocation. Named third-party MCP clients and deployed proxy behavior still require their own live qualification.
