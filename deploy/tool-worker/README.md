# Pinned AX tool-worker image

This build joins the already verified AX runner with the exact Codex, Claude
Code, and OpenCode npm lockfile. It installs Git from a dated Debian snapshot
**at image build time**, runs all three exact version checks, and emits a
machine-readable binary/hash/version manifest. It does not install packages,
choose `latest`, fetch credentials, or call a model when an AX task starts.

Build on native Linux/amd64 with AVX2, Go 1.27.1, Docker Buildx or Buildah
1.42.1, Python 3, Git, and network access to the pinned Node/npm inputs and
Debian snapshot.
Use a clean, committed Blaxsmith checkout and the supported clean AX checkout:

```sh
bash integrations/ax/build.sh ../reference/ax /tmp/blaxsmith-ax-verified
bash deploy/tool-worker/build.sh /tmp/blaxsmith-ax-verified /tmp/blaxsmith-tool-image
```

On the AX development node, the verified AX build and Blaxsmith source are
already present. Build the same image with Buildah, without Docker:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
bash deploy/tool-worker/build.sh /opt/blaxsmith-dev/ax-provider-20260923 \
  /opt/blaxsmith-dev/tool-worker-image-20260923 --buildah \
  --git-ca /opt/blaxsmith-dev/private-git-fixture/ca.pem
```

The Buildah path uses the same Dockerfiles, pinned package lock, AX
provenance checks, offline CLI verification, and `proof.json` format. Its
`--network host` build option accommodates the development node's DNS setup;
the CLI installation and final manifest check still run without network.

`build.sh` first compares the AX binary, patch hashes, and Git askpass helper
to `provenance.json`. It compiles the static `blaxsmith-tool-worker` from the
current commit, builds the [pinned CLI proof](../runtime-proof/README.md),
then adds the AX runner, worker, and Git to the final image. It also installs
`tmux` (the build fails below 3.2, which viewers need for `attach -f
ignore-size`) and util-linux `script` for the live terminal, and links
`/usr/local/bin/bx` to the worker ([interactive sessions](../../docs/interactive-sessions.md)). The final image
pre-owns `/run/blaxsmith` as mode 0700 for the bootstrap credential file.
Pass `--git-ca PEM` when the AX Workspace Git origin uses an administrator
managed private CA; the image combines that PEM with the pinned base trust
bundle at `/etc/ssl/certs/blaxsmith-git-ca.pem`. Its hash is recorded in
`proof.json`. Without it, the image still provides the system root bundle at
that AX-owned path.
`proof.json` records the local image manifest digest, AX provenance hash,
package-lock hash, image IDs, and `cli_manifest` with each absolute CLI binary
path, SHA-256, and exact version. `--check` verifies clean source and AX
provenance without building an image:

```sh
bash deploy/tool-worker/build.sh /tmp/blaxsmith-ax-verified /tmp/unused --check
```

The local manifest digest is **not a registry pull reference**. Before an
approved runtime is used, publish the tested image to a registry, record its
registry `repo@sha256:...` digest, and create an administrator approval for
each intended harness/model/effort pair. Populate `tooladapter.Runtime.Image`
from that registry reference and its `Binary`, `BinarySHA256`, and `Version`
from `cli_manifest`; the approved `Supported` pairs are explicit policy, not
guessed from the package catalog. Pull and inspect the published digest before
enabling AX dispatch. The Buildah image can be published to the node's
loopback development registry with the tag from `proof.json.local_tag`:

```sh
bash deploy/tool-worker/publish-buildah.sh \
  /opt/blaxsmith-dev/tool-worker-image-20260923 \
  127.0.0.1:5001/blaxsmith-tool-worker:proof-<fingerprint>
```

`publish-buildah.sh` rechecks the local image digest, rejects a different or
moving tag, and writes the pullable digest to `registry-proof.json.image`.
The loopback registry uses plain HTTP and is for this development node only.
Neither script deploys to the cluster or creates administrator approvals.

## Runner variants

`--variant NAME` layers extension runtimes onto the verified worker image
with `deploy/tool-worker/NAME.Dockerfile`, pinned by `NAME-runtimes.json`.
The `guild` variant adds uv 0.12.18 (release tarball with a pinned SHA-256),
the Python 3.12 interpreter that uv version selects, and `serena-agent`
1.7.0 resolved with `--exclude-newer 2026-09-01T00:00:00Z`. It sets
`UV_PYTHON_DOWNLOADS=never` for tasks.

```sh
bash deploy/tool-worker/build.sh /tmp/blaxsmith-ax-verified /tmp/blaxsmith-guild-image --variant guild
bash deploy/tool-worker/build.sh /tmp/blaxsmith-ax-verified /tmp/unused --check --variant guild
```

`check-variant.py` runs first, including in `--check`: it refuses pins that
differ from the extension manifest's declared `runtimes`, a Dockerfile that
does not use them, or a floating `latest`. The variant image is tagged
`blaxsmith-tool-worker:proof-<fingerprint>-<variant>`; the fingerprint covers
the variant files. `cli_manifest.runtimes` records each runtime's version,
path and SHA-256 (verified offline by `runtimes.mjs` at build time), and
`proof.json.variant` names the variant. Until AX accepts a runner image
allowlist, extension stages need the variant to be the configured runner
image.

## Process model

The AX runner currently serves port 80 and this combined image therefore
keeps its existing root process model inside the gVisor sandbox. A rootless
runner needs a separate AX port/ownership change. The image only supplies the
credential file location and tool binaries; the product connector must deliver
authorized leases before the command runs. The lease schema and AX envelope
support one Git plus one model capability together, but product Git-binding
selection and per-attempt Git Workspace wiring are still unfinished.

The APT source follows the [Debian snapshot format](https://snapshot.debian.org/)
with `check-valid-until=no`. It uses HTTP for the initial package metadata
because the slim base image lacks a CA bundle; APT still validates Debian's
signed `InRelease` and package hashes. The build records the final digest using
[Buildx metadata](https://docs.docker.com/reference/cli/docker/buildx/build/)
or Buildah image inspection.
