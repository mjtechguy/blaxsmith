# Pinned Linux/amd64 CLI image proof

This image is a credential-free packaging proof for **one tested selection**:

| CLI | Root npm package | Required installed native package | Observed `--version` |
|---|---|---|---|
| Codex | `@openai/codex@0.156.1` | `@openai/codex-linux-x64@0.156.1-linux-x64` (npm alias to `@openai/codex@0.156.1-linux-x64`) | `codex-cli 0.156.1` |
| Claude Code | `@anthropic-ai/claude-code@2.1.280` | `@anthropic-ai/claude-code-linux-x64@2.1.280` (glibc) | `2.1.280 (Claude Code)` |
| OpenCode | `@opencode/cli@2.0.14` | `@opencode/cli-linux-x64@2.0.14` | `opencode v2.0.14` |

`package.json` selects exact versions, and `package-lock.json` pins each
tarball's SHA-512 integrity, including native optional dependencies. On the
pinned Node 24.21.0 Debian/glibc image, `npm ci` includes optional packages
with all lifecycle scripts suppressed. The verifier rejects a missing or
mismatched native package before running any CLI. The trusted build then runs
only Claude Code's pinned `install.cjs` and OpenCode's pinned `postinstall.mjs`
to stage their binaries. OpenCode's postinstall is forced offline so its npm
fallback cannot fetch a package outside the lockfile. Build and final-image
smokes check all three exact version outputs. The image defaults to UID/GID
10001 and contains no model, Git, registry, or cluster credential.

Build from the repository root on **native Linux/amd64 with AVX2**:

```sh
buildah bud --format oci --arch amd64 \
  -t blaxsmith-runtime-proof:0.1.0 \
  -f deploy/runtime-proof/Dockerfile .
```

The 2026-09-23 proof used Buildah 1.42.1 on the dedicated Ubuntu 26.04
Linux/amd64 node. Its default build network could not resolve npm, so that
run added `--network host`; no model or Git credential was supplied. The
Node base image is pinned to
`sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6`.
Buildah installed nine platform-eligible npm packages. A fresh image
container ran as UID 10001 and repeated all three `--version` checks. In a
separate disposable container, deleting the OpenCode native package made
`verify.mjs` fail with `@opencode/cli-linux-x64@2.0.14 missing or mismatched`.

| Evidence | SHA-256 |
|---|---|
| `package-lock.json` | `0676d33b5e2029e38db0bdd162f393488ca816c0b028c5967db2172ca2e1551c` |
| OCI image manifest | `b709ae909f9e162d240463bff0fa171ac5b6afaf9b7345cfd19fbc07f1870e86` |
| OCI archive | `d88627c70179d78d23691fb48cfe9768b5bfc3e47132dd632a8732d0fa339b59` |

The 706 MiB archive is retained on the development node at
`/root/blaxsmith-runtime-proof/runtime-proof-linux-amd64.oci.tar`; it is not
published to a registry. The manifest digest is for that archive, not a
registry pull reference. A release build must record its newly pushed digest
and pass the separate license/distribution review.

This proof says nothing about model login, native delegation controls,
structured events, cancellation/resume, AX launch, or safe self-update
suppression. The selected versions do not follow moving `latest` at launch.
OpenCode's optimized x64 binary was verified on AVX2 hardware; baseline and
other platform variants need separate evidence before scheduling them.
