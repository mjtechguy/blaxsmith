# Machine API and MCP

Start with the executable [factory quickstart and client matrix](factory-integration.md). Optional Guild details live in the [Guild adaptation guide](guild-integration.md).

Blaxsmith exposes project-scoped delegated user and service credentials through **Project → Settings → Agent access**. Browser operations continue to require cookies, Origin and CSRF. Machine operations use the same workflow service and domain stores through `/machine/blaxsmith.api.v1.WorkflowService/`, with a Blaxsmith bearer token and no browser cookies or Origin header.

## Credentials

Select a project, label, expiration (1 hour–30 days), and explicit scopes. Copy the token from the creation response; only its SHA-256 digest is persisted. Listing credentials never returns the secret. Issuance is browser-only; machine credentials cannot mint or manage other credentials. If an issuance response is lost, inspect the inventory and revoke the uncertain credential before creating another.

Each delegated user credential has a dedicated API session bound to its issuing principal, organization, project and original role. Every request rechecks expiry, revocation, live membership, role, login policy and MFA policy. Role changes invalidate the credential rather than expanding its authority. Domain mutations reuse the existing transaction-level session fences; revocation updates that same session. Revoking an API credential does not revoke the issuer's browser session. Browser sign-out does not revoke separately issued API credentials. Existing runs retain their own access and stop semantics; revocation is not proof that a worker stopped.

| Scope | Operations |
| --- | --- |
| `project.read` | Project/source/check reads, goals/plans, runs/tasks/events, interactions, review and evidence |
| `goal.write` | Create goals, append context/answers, save plan versions, record already accepted checkpoints |
| `run.launch` | Planning launch, preview and run launch; existing model grants and runtime readiness still apply |
| `run.control` | Answer run interactions, steer attempts and control authorized goals |
| `review.decide` | Decide current review packages; also required alongside `run.control` for approval interactions |

Terminal takeover, secret access, administrative operations, project creation, connection management and credential management are excluded from the machine projection. Viewer credentials can only receive `project.read`. Delegated mutations record the credential session in the identity audit stream as a received request, separately from domain success. Service identities are described below. Attempt-scoped child credentials remain separate roadmap work.

Use `DescribeMachineAccess` to inspect the effective immutable project/scopes and expiry. `GetPlatformCapabilities` describes supported features; it does not confer grants. Unknown methods are denied by default. Object ownership is checked against the credential's project before invoking domain operations. Requests cannot supply an organization identity.

```sh
# Set the token through your secret manager or protected environment.
curl --fail-with-body 'https://blaxsmith.example/machine/blaxsmith.api.v1.WorkflowService/DescribeMachineAccess' \
  -H "Authorization: Bearer $BLAXSMITH_API_TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}'
```

The generated Go/TypeScript WorkflowService clients work with this base path and an Authorization interceptor. Never send provider API keys as platform credentials. Transport errors preserve Connect codes; domain errors remain bounded. The API refuses redirects only in the bundled MCP client; configure the same no-redirect policy in custom clients to avoid forwarding credentials to another origin.

## MCP

Build the CLI and configure a local MCP host to execute:

```sh
blaxsmith mcp --server https://blaxsmith.example
```

Supply `BLAXSMITH_API_TOKEN` through the MCP host's protected environment. The bridge uses the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), pinned in `go.mod`, for stdio protocol handling. It validates the server origin, verifies TLS, refuses redirects, and uses only the machine API. It needs no browser cookies, database access, AX credentials, or provider secrets.

Tools are named `blaxsmith_` followed by the WorkflowService method, for example `blaxsmith_GetGoal`, `blaxsmith_CreateGoal`, `blaxsmith_PreviewRun`, and `blaxsmith_EventsAfter`. The exposed tool list is filtered by the credential's scopes at startup. Each call reauthenticates at the API, so a connected MCP session loses authority immediately after revocation. Tool arguments use protobuf JSON names and revision strings; schemas are derived from the same protobuf descriptors. There is no arbitrary method execution tool.

The `blaxsmith://instructions` resource and server instructions explain ownership, idempotency, cost-bearing launch, durable questions, evidence, and cancellation. Long work returns durable run IDs; clients poll status/events. No MCP Tasks extension, automatic elicitation or OAuth authorization flow is advertised. Stdio hosts can inspect and answer durable interactions through the tools. Stopping the bridge does not cancel a run.

## Agent workflow

1. Describe machine access and platform capabilities.
2. Read the project, source and check policy; create or inspect a goal.
3. Resolve questions, save bounded plan versions using expected revisions and a stable request key.
4. Preview the selected plan or neutral recipe. Review blockers, source, assignments and policy.
5. Launch only within delegated authority, using the preview hashes and a stable launch key.
6. Inspect tasks, events, interactions and evidence. After a timeout, reconcile durable IDs before issuing new work.
7. Keep reported results, observed checks and acceptance separate. Review decisions require the separate scope and exact package identity. Merge/deployment are not implied.

## Qualification

`make e2e` runs the actual HTTPS app with disposable PostgreSQL schemas, builds the real frontend, exercises browser goal/plan/credential flows and starts the actual CLI as an MCP subprocess. It covers idempotency conflicts, stale revisions, scope denial, cross-project denial, expired/revoked tokens, cookie ambiguity, plan-cycle rejection, draft preservation and reload. It also runs the local Git/correction-loop qualification fixture.

Set `BLAXSMITH_TEST_DATABASE_URL`. Chrome is the default Playwright channel; set `BLAXSMITH_E2E_BROWSER_CHANNEL` for an installed supported channel. The fixture never invokes paid providers or a live AX cluster. Live provider/runtime, upgraded AX networking, remote OAuth authorization and platform-managed delegation require their own remaining qualification.

## External factory goals

`CreateGoal` defaults to shipped Anvil. External clients can supply `factory_id`, `factory_version`, and up to 16 typed `questions` (ID, title, Markdown body, options, multi-select, free text and blocking preference). Factory identity is attribution, not an executable plugin selector or extra authority. The ordinary project and scope checks apply.

An external factory saves its own JSON through `SaveGoalPlan`. Its `schema_version` must start with `<factory_id>/`, for example `example-factory/plan/v1`. Content is bounded to 256 KiB, canonicalized without losing large JSON numbers, versioned and bound to the exact goal revision. The platform does not claim semantic validation of another factory's document. Anvil plans continue through Anvil's graph, reference and coverage validator. External artifact import and run association use the explicit factory recipe binding described below.

Native planning and compilation reject external or unsupported factory identities. The browser renders external plans as data, supports JSON revisions, and omits Anvil launch controls. External factories compose their own authorized recipes through preview and launch. Guild therefore remains optional; it does not define the goal store or native plan contract.

`ListProjectVerificationHistory` and `GetDeliveryReport` are available to `project.read` credentials through both HTTPS and MCP. Neither endpoint confers mutation or delivery authority.

## Binding a factory recipe to a goal

Pass `goal_context: { goal_id, expected_revision, plan_version }` to both `PreviewRun` and `LaunchRun`, alongside the ordinary Git recipe path or granted library version. Use `plan_version: 0` for planning; use a saved version for execution. This is exclusive with Anvil's `goal_execution`. Launch requires both preview digests.

The recipe must declare the exact goal factory ID/version, and include `.blaxsmith/platform/goal.json` in `documents`. Execution also declares `.blaxsmith/platform/selected-plan.json`. The platform supplies bounded frozen bytes and source attribution at those paths without modifying the recipe. Git collisions, undeclared platform files, stale context, wrong project, missing plans and changed digests are rejected. Admission rechecks the association under the existing transaction fence; a context change after preview cannot silently launch the previous assignment.

A completed planning stage can publish a `factory-plan` artifact. `SaveGoalPlan` imports its `evidence_id` only from a completed current attempt associated with that goal/context. Subsequent edits retain provenance and their own content digests. Anvil uses its own `anvil-plan` artifact and schema validator. Associated planning/execution runs appear in the goal workspace and delivery export.

The local conformance test follows a real authenticated external goal and plan through exact context resolution, real Git freezing and PostgreSQL admission, then reads the association through the machine API and rejects stale admission. End-to-end external runtime/provider qualification remains open.

Credential inventory prioritizes live credentials. Each user can hold up to 100 active credentials per project; revoke one before issuing more. Expiration uses database time, and out-of-range lifetimes are rejected before conversion.

Goal admission and lifecycle controls are documented in [goal control](goal-control.md) and [goal allowances](goal-allowances.md). `GetGoalControl` is readable with `project.read`; `ControlGoal` requires `run.control`, goal ownership/organization authority, a control version and stable request key. Machine clients cannot increase goal allowances.


## Service identities

Organization owners/admins can create a project-bound service identity in **Agent access → Add service identity**, select it under **Act as**, and issue scoped, expiring credentials. Creating the identity uses a stable request key; rotating its credentials preserves the principal ID and ownership of its goals. A project permits up to 100 retained service identities and up to 100 active credentials per identity. Credentials still expire after at most 30 days and only token digests are stored.

Service principals have no password, email or browser login. They have member authority, narrowed by their credential's project and scopes; they use project model grants rather than a human's personal account. Creation, issuance and disable actions are attributed to the human administrator; work is attributed to the service principal. `ApiToken.principal_id` and `kind` distinguish service and delegated-user credentials.

Service sessions record `auth_method=service`, `mfa_level=none`, and `credential_kind=service`. They do not claim a human MFA event or depend on the issuing person's later browser session. Organization human login/MFA settings still govern administrators issuing credentials; service requests use their dedicated live membership/session fences. Changing membership role invalidates issued credentials. Disabling the service revokes all its sessions. This closes API authority, not already-running workers; use goal/run cancellation separately.

Only browser-authenticated organization administrators can create/list/disable service identities or issue/revoke their credentials. Services cannot mint credentials, edit allowances, manage connections, take over terminals, or receive `review.decide`. Required human review stays human; an explicitly selected policy-acceptance recipe can still use its normal evidence checks. Database constraints prevent attaching a password or human session to a service principal.

Administrative API methods are `CreateServicePrincipal(project_id,label,request_key)`, `ListServicePrincipals(project_id)` and `DisableServicePrincipal(principal_id)`. `CreateApiToken` and `ListApiTokens` accept an optional `service_principal_id`; empty retains delegated-user behavior. These operations are deliberately absent from machine/MCP tools.

## Remote MCP with provisioned credentials

The HTTPS application also serves sessionless Streamable HTTP at **`/mcp`**. Configure a server-side MCP client with that URL and `Authorization: Bearer <Blaxsmith credential>`. The existing application TLS and exact-host checks apply. The endpoint rejects cookies and Origin headers, has a 1 MiB request limit and bounded API responses, and returns JSON MCP responses. It provides no standalone SSE, server-initiated elicitation or session-local work state. Durable goals, runs, interactions and events remain the reconnection mechanism.

Every HTTP request builds its tool projection from its own live credential. Tool calls pass through the same Connect machine handler, project/scope interceptor, audit path and transaction fences as direct API and stdio calls. No loopback network credential forwarding or retained cross-request MCP authority is used. Revoked/expired credentials are refused even if the client had already initialized. The endpoint accepts provisioned Blaxsmith tokens and, when configured, resource-bound tokens from the [OAuth flow](mcp-oauth.md). Cross-origin browser MCP remains unsupported.

The integrated test uses the official SDK against the real HTTPS endpoint, compares scoped tools with stdio, retries the same creation across transports, verifies missing credentials/untrusted Origin rejection and checks revocation on an initialized client. Optional [OAuth authorization](mcp-oauth.md) adds pre-registered public clients and explicit browser consent; named third-party client qualification remains open.

Usage reads (`GetRunUsage`, `GetGoalUsage`) require `project.read` and preserve exact decimal totals and unknown coverage across browser, Connect, stdio MCP and HTTPS MCP. See [reported usage](usage-reporting.md); these reads do not certify billing or authorize additional spending.

Accepted-run history uses shared [goal checkpoints](goal-checkpoints.md). Recording one preserves existing acceptance; it cannot grant it.

## Generated operation reference

[Machine API contract](machine-api-contract.json) is generated from the compiled WorkflowService descriptors, the explicit machine allowlist and the same operation descriptions/input schemas as MCP. Each operation includes its HTTPS path, MCP name, required scope, project-bound resource selector, request schema and response schema. It includes all potentially supported machine operations; live `DescribeMachineAccess` remains authoritative for the credential's actual access.

Run `blaxsmith api-contract` without credentials, network or a database to inspect the installed binary's contract. Run `make api-reference` to update the checked-in reference; the HTTPS/MCP E2E test invokes the built CLI offline and rejects reference drift. Business validation, grants, expected revisions and required fields remain enforced by the shared API; structural schemas alone do not establish launch feasibility. Consult the workflow protobuf comments and the operation guides for value limits and side effects.
