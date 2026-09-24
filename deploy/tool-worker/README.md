# Pinned AX tool-worker image

This build joins the already verified AX runner with the exact Codex, Claude
Code, and OpenCode npm lockfile. It installs Git from a dated Debian snapshot
**at image build time**, runs all three exact version checks, and emits a
machine-readable binary/hash/version manifest. It does not install packages,
choose `latest`, fetch credentials, or call a model when an AX task starts.

Build on native Linux/amd64 with AVX2, Go 1.27.1, Docker Buildx, Python 3,
Git, and network access to the pinned Node/npm inputs and Debian snapshot.
Use a clean, committed Blaxsmith checkout and the supported clean AX checkout:

```sh
bash integrations/ax/build.sh ../reference/ax /tmp/blaxsmith-ax-verified
bash deploy/tool-worker/build.sh /tmp/blaxsmith-ax-verified /tmp/blaxsmith-tool-image
```

`build.sh` first compares the AX binary, patch hashes, and Git askpass helper
to `provenance.json`. It compiles the static `blaxsmith-tool-worker` from the
current commit, builds the [pinned CLI proof](../runtime-proof/README.md),
then adds the AX runner, worker, and Git to the final image. The final image
pre-owns `/run/blaxsmith` as mode 0700 for the bootstrap credential file.
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
enabling AX dispatch. No registry push or cluster deployment happens in this
script.

The AX runner currently serves port 80 and this combined image therefore
keeps its existing root process model inside the gVisor sandbox. A rootless
runner needs a separate AX port/ownership change. The image only supplies the
credential file location and tool binaries; the product connector must deliver
an authorized provider lease before the command runs. Private Git plus a
provider key in one bootstrap release remains blocked by the one-lease schema.

The APT source follows the [Debian snapshot format](https://snapshot.debian.org/)
with `check-valid-until=no`; the build records the final digest using
[Buildx metadata](https://docs.docker.com/reference/cli/docker/buildx/build/).
