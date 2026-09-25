# Pinned CLI adapter slice

`internal/tooladapter.Prepare` accepts an administrator-approved runtime image,
absolute executable path and SHA-256, exact CLI version, and an explicit list of
supported model/effort pairs. It rejects a Git recipe's unapproved selection,
implicit OpenCode `provider-default`, custom instructions/skills that have not
been mounted, missing image digest, or unbounded timeout/output. It never
chooses moving `latest` or a substitute model/tool; Claude requires a full
model name and gets a one-model allowlist with no configured fallback. An AX task must
execute `Run` inside the image; dispatching the CLI directly would bypass its
bounds and checks. The existing AX runner-image bootstrap gate must pin that
exact digest. The npm catalog and the credential-free
`deploy/runtime-proof` image are not approval records.

`Run` is a pod-side primitive: it checks the executable hash and exact
`--version` output before launch, starts with an isolated home/config and no
ambient environment, accepts only the scoped lease's credential variables (`CODEX_API_KEY` for a
native Codex key, since `codex exec` ignores `OPENAI_API_KEY` for its built-in
provider; `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `OPENCODE_API_KEY`, and a
connection base URL's public `ANTHROPIC_BASE_URL`; see "Base URLs" in
`docs/access-authority.md` for the Codex and OpenCode equivalents), disables Claude updates, writes OpenCode's global
`update: disable` setting, blocks project-local Codex/OpenCode configuration,
and bounds elapsed time plus combined stdout/stderr.
It returns process bytes and an error; it cannot decide that a task passed.
The enclosing AX pod must enforce the image digest, read-only tool filesystem,
network policy, cgroup/process cleanup, and credential lease revocation. No
authenticated AX launch is wired yet, and no repository config/plugin/tool
capability has been approved for model execution. The current adapter blocks
declared instruction/skill files until their scoped loading is implemented.
Provider-side remapping and the actual model reported by a CLI event stream
still need verification before a run can be credited to the selected recipe.

## AX task command

`axbridge.Bridge.Tool` now accepts one frozen `tooladapter.Request` for an
attempt. It rejects an attempt or runner-image mismatch, validates the exact
model/effort against the approved runtime record, and places only the public
selection in `spec.command`: `/usr/local/bin/blaxsmith-tool-worker` followed
by one JSON argument. The command hash in the durable runtime binding and
signed AX exit readback is derived from those exact argv bytes. A nil `Tool`
retains the synthetic probe while product scheduling is being connected.

The worker must be installed in the **same digest-pinned AX runner image** as
`ax-task-runner`, the CLI binaries, and Git. After AX's pre-command bootstrap
gate, it reads `/run/blaxsmith/agent-credential.json` (regular mode 0600 file)
with `attempt_id`, `provider` (`openai` or `anthropic`), `expires_at` (Unix
seconds), and `api_key`. The provider must match the selected harness/model;
the lease must be live and expire within an hour; the process deadline is
clamped to that expiry. No ambient environment is
passed to the CLI. The worker passes the key only to the actual tool process,
redacts direct occurrences from output, and exits nonzero on absent or invalid
credential, binary/hash/version mismatch, unsupported selection, or tool error.

The attested AX release now accepts one provider credential as an alternative
to Git setup. `access.PreflightModelInvoke` checks an exact `model.invoke`
grant and encrypted secret before attempt reservation; `BindModelInvoke`
freezes the grant/policy versions on the attempt. At release,
`AuthorizeModelInvoke` rechecks connection, grant, binding, project policy and
provider under locks, while `ReserveModelLease`, `MarkLeaseAttempt`, and
`MarkLeaseDelivered` record the delivery. The signed encrypted envelope binds
the key to the live actor challenge. The AX runner rejects a mismatched tool
command and writes only the worker's private credential file, then removes it
on command exit. No raw key goes into AX Task metadata.

The lease table now permits **one Git and one model capability per challenge**;
each keeps a separate binding and lease while AX seals both in one challenge-bound
envelope. The pinned AX overlay and combined Git/model path have passed the
overlay build and security tests. Public dispatch now creates a per-attempt AX
Git Workspace, fetches the frozen commit object ID rather than a mutable branch,
and the worker checks the mounted checkout against the frozen URL and commit.
An exact shallow fetch was confirmed against GitHub's public Git transport.
Product selection of a private Git binding and a
browser-launched private-repository task are still missing. A recorded
revocation fences future delivery; an already issued raw provider key remains
usable until provider rotation, expiry, or actor termination. The scheduler
must stop the actor on grant/connection revocation. An approved registry image
and product connector callbacks are still required before live use. A CLI exit only proves process
exit; evidence collection, actual model identification, and a trusted
supervisor boundary remain required before crediting work or enabling
untrusted repositories.

The noninteractive flags follow the publishers' current [Codex CLI/security
guidance](https://learn.chatgpt.com/docs/agent-approvals-security), [Claude Code
CLI reference](https://code.claude.com/docs/en/cli-reference), and [OpenCode v2
commands](https://opencode.ai/v2/docs/cli/commands/),
[models](https://opencode.ai/v2/docs/models), and
[configuration](https://opencode.ai/v2/docs/config), plus Claude Code's
[model fallback and allowlist rules](https://code.claude.com/docs/en/model-config).
A new CLI release needs a
version-specific capability and smoke check before it becomes an approved
runtime; the adapter does not infer compatibility from a release number.
