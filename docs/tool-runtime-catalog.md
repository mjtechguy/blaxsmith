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
checked. It does not yet cache conditional responses, attest binary assets,
check Linux/architecture compatibility, disable self-updates, or bind a
release to an adapter/profile. A failed live fetch fails visibly instead of
reusing an unlabelled stale result. Those are P0-12 and P1-24 follow-ups.

In the 2026-09-23 UTC live probe, publisher metadata selected Codex `0.156.0`,
Claude Code `2.1.280` (`stable` tag `2.1.267`), and OpenCode v2 `2.0.14`.
These are observations at that fetch time, never static installation defaults.
