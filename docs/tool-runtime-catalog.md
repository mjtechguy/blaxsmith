# Tool runtime release catalog: first reader

`blaxsmith tools` fetches the current npm package metadata for the three
explicitly supported launch tools and prints the newest 20 non-prerelease
versions per tool by default. `--limit` accepts 1–100; `--tool` accepts
`codex`, `claude-code`, or `opencode`. Each result includes its package, fetched
time, publisher `latest` and `stable` tags when present, publication time,
tarball URL, and npm SHA-512 integrity value. `latest_stable` selects the
highest non-deprecated numeric `major.minor.patch` version. It does not treat
Claude Code's delayed publisher `stable` channel as the newest release.

```sh
go run ./cmd/blaxsmith tools --limit 5
go run ./cmd/blaxsmith tools --tool opencode --limit 20
```

The package allowlist follows the publishers' installation documentation:
[Codex](https://learn.chatgpt.com/docs/changelog),
[Claude Code](https://code.claude.com/docs/en/getting-started), and
[OpenCode v2](https://opencode.ai/v2/docs). OpenCode v2's official npm package
is `@opencode/cli`; the earlier `opencode-ai` package belongs to a different
major line. [OpenCode's migration guide](https://opencode.ai/v2/docs/migrate-v1)
documents breaking server/plugin/client contracts. Claude Code documents that
its `stable` channel is deliberately delayed relative to `latest`; the catalog
shows both names and versions without collapsing them.

The reader is read-only. It does not approve, download, verify, install, or
run any version; the npm integrity field is metadata until actual bytes are
checked. `internal/catalog.DownloadVerified` now provides a trusted-builder
primitive: it accepts only an exact allowlisted npm tarball URL and SHA-512 SRI,
streams at most 200 MiB into an owner-only temporary file, and returns its
SHA-256 only after integrity succeeds. A live opt-in probe verified the latest
root tarballs for all three tools on 2026-09-23 UTC. This does **not** verify
an installable runtime: those root packages are small launchers and declare
platform-native optional dependencies. The [Codex release builder](https://github.com/openai/codex/blob/main/codex-cli/scripts/build_npm_package.py)
shows the separate Linux packages. A runtime image must resolve and verify the
selected Linux/architecture dependency, ensure it was actually installed,
disable or constrain updates, and smoke-test the exact executable before
promotion; an npm exit code or verified root tarball alone is insufficient.

The CLI always fetches live metadata. The web preview coalesces
concurrent requests, caches success for 15 minutes, and serves the last success
for at most 24 hours during an upstream failure with a visible stale label and
one-minute retry backoff. It does not yet use conditional requests or a durable
scheduled catalog, attest binary assets, check Linux/architecture
compatibility, disable self-updates, or bind a release to an adapter/profile.
Those are P0-12 and P1-24 follow-ups.
The separate [credential-free Linux probe](tool-cli-probe.md) confirms basic
version/help entry points for one exact selection from each package.
The [pinned Linux/amd64 image proof](../deploy/runtime-proof/README.md) now
checks the native optional packages, lockfile integrity, and exact CLI versions
for a later tested selection. It is not promoted to an agent runtime.

In the 2026-09-23 UTC live probe, publisher metadata selected Codex `0.156.0`,
Claude Code `2.1.280` (`stable` tag `2.1.267`), and OpenCode v2 `2.0.14`.
These are observations at that fetch time, never static installation defaults.
