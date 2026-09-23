# Credential-free Linux CLI packaging probe

On 2026-09-22 local time, a one-shot Kubernetes Job ran on the dedicated
Ubuntu 26.04/amd64 k3s node. It used
`node:24-bookworm-slim` resolved to
`docker.io/library/node@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6`.
The pod had no service-account token, ran as UID/GID 10001, dropped Linux
capabilities, disallowed privilege escalation, used a read-only root
filesystem and RuntimeDefault seccomp, and wrote only to bounded temporary
volumes. No model or Git credential was supplied. The temporary namespace was
deleted after the successful Job and logs were inspected.

| Tool | Exact npm package | Observed version | Relevant help surface |
|---|---|---|---|
| Codex | `@openai/codex@0.156.0` | `codex-cli 0.156.0` | `exec` noninteractive command and `--model` flag |
| Claude Code | `@anthropic-ai/claude-code@2.1.280` | `2.1.280 (Claude Code)` | `--print`, `--output-format=stream-json`, and `--model` |
| OpenCode v2 | `@opencode/cli@2.0.14` | `opencode v2.0.14` | `run`, `acp`, and `models` commands |

`npm install --prefix /work --no-save --no-audit --no-fund` added nine
packages in 20 seconds. npm 11 warned that the Claude Code and OpenCode
postinstall scripts were not on its `allowScripts` list; the version/help
commands still ran. Before a release build, verify whether those scripts are
required for each supported platform and approve them only within the pinned
build process. The tool package tarballs were selected by exact versions here,
but this probe did not independently hash the downloaded bytes or pin the
transitive packages in a lockfile.

This establishes packaging and basic CLI entry points only. It does not prove
supported authentication, actual headless execution, structured event
semantics, native delegation controls, resume/cancel, model availability,
agent permissions, or AX integration. Those are P0-06/11 and P1 adapter gates.
