# Single-node AX development environment

These are tested development inputs for the dedicated AMD64 Linux node supplied
on 2026-09-22. They are not the product Helm chart or an enterprise installer.
The node runs k3s, Agent Substrate, and AX; Blaxsmith's recipe compiler runs locally.
No model account, Git credential, tenant data, or private source was sent to a
worker. Reference sources on the node live under `/opt/blaxsmith-dev`.

## Installed baseline

- Ubuntu 26.04.1 / AMD64; Go 1.27.1, verified against the official download checksum.
- k3s `v1.36.4+k3s1`; `k3s-config.yaml` installed as `/etc/rancher/k3s/config.yaml`.
  Certificate feature gates and the beta API are required by this Substrate pin.
- Substrate commit `672533541dbfcd29084e4de2475267088bda3651` with exactly one
  deployment edit: `kind-registry:5000` becomes
  `registry.blaxsmith-build.svc:5001` in
  `manifests/ate-install/kind/atelet/kustomization.yaml`. Its build/node label is
  consequently `67253354-dirty`; the Go source is unchanged.
- AX source is fetched from `google/ax` and pinned at
  `f009cc81c9a571073bc1dd58cd2ed934bf2d5b1c`. The source checkout remains
  clean; the deployed controller and runner use the tested Blaxsmith overlay
  against that revision. The build verifies the exact pin and records its
  provenance in [`integrations/ax/README.md`](../../integrations/ax/README.md).
- The pinned Substrate tool module supplies ko 0.19.1. Images are built on the
  remote node and retained in the development registry; resolved deployment
  manifests use image digests. [Observed images](../../docs/runtime-images.json)
  include the upstream third-party images used during the test.

## Node setup and access

The k3s installation used the `install.sh` from its exact release tag with
`INSTALL_K3S_VERSION=v1.36.4+k3s1`. The Kubernetes config and kubeconfig are mode
0600. Use SSH and run `kubectl` on the node; no public Kubernetes access is needed.
Do not copy the kubeconfig, SSH private key, or bootstrap tokens into this repo.

The host's UFW policy allows TCP 22 and pod/service CIDRs `10.42.0.0/16` and
`10.43.0.0/16`, denies other incoming connections, and permits outgoing traffic.
`firewall.nft` additionally rejects new connections forwarded from public NIC
`eth0` before Kubernetes' forwarding rules, covering pod hostPort/NodePort DNAT.
It is installed at `/etc/blaxsmith-dev-firewall.nft` and loaded by
`blaxsmith-dev-firewall.service` before k3s. Review the NIC name before reuse.

`registry.yaml` installs a single registry with a local PVC and host port 5001;
its debug port binds loopback 5002. `registries.yaml` is installed in
`/etc/rancher/k3s/registries.yaml` so containerd can pull local builds over HTTP.
The registry is protected by the node firewall and is for this trusted dev node.
External IPv4 probes confirmed 22 reachable and 6443, 5001, 5002, 8085, 9090,
and 10250 unreachable. External IPv6 probing was unavailable from the workstation;
the UFW IPv6 and nftables `inet` rules are configured but that path is not proved.

## Authenticated Blaxsmith preview

The product app runs in `blaxsmith-preview`; its repeatable, non-secret Helm
values are in [`preview-app-values.yaml`](preview-app-values.yaml). The existing
NodePort front end serves `https://135.181.34.21/login`. Render and apply an app
release from the repository root with:

```sh
helm template preview deploy/charts/blaxsmith-app \
  --namespace blaxsmith-preview -f deploy/dev/preview-app-values.yaml |
  ssh root@135.181.34.21 kubectl -n blaxsmith-preview apply -f -
```

This development installation has AX dispatch enabled and explicitly selects
`open-dev` egress so model and Git endpoints are reachable. Use synthetic or
public work only; it does not establish production egress enforcement. The AX
and Substrate credentials are pre-created namespace Secrets and never belong in
this file. Provider model keys still come from the authenticated UI.

## Rebuild the runtime on this prepared node

Use checked-out source revisions above, the installed registry, and the copied
files from this directory under `/opt/blaxsmith-dev`. The commands below operate
on the dedicated dev cluster only.

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export NO_DEV_ENV=true ATE_INSTALL_KIND=true KUBECTL_CONTEXT=default
export KO_DOCKER_REPO=127.0.0.1:5001 KO_DEFAULTPLATFORMS=linux/amd64
export BUCKET_NAME=ate-snapshots

cd /opt/blaxsmith-dev/substrate
# Apply the one registry-address edit described above before building.
./hack/install-ate.sh --rollout-timeout 60s --deploy-ate-system
go build -o /usr/local/bin/kubectl-ate ./cmd/kubectl-ate
blaxsmith_ko=$(./hack/run-tool.sh --print-bin-path ko)
./hack/run-tool.sh ko build --platform=linux/amd64 \
  --ldflags="$(make -s ldflags)" ./cmd/ateom-gvisor \
  > /opt/blaxsmith-dev/worker-image.txt

cd /opt/blaxsmith-dev/ax
go build -o /usr/local/bin/ax ./cmd/ax
"$blaxsmith_ko" resolve -f deploy > /opt/blaxsmith-dev/ax-rendered.yaml
```

Before applying AX manifests, replace its `ClusterRole`/`ClusterRoleBinding` with
`Role`/`RoleBinding` (including `roleRef.kind`) and apply in namespace `ax-system`.
This constrains AX's upstream secret lookup to its own namespace. The installed
controller has **no cluster-wide secret listing permission**, verified with
`kubectl auth can-i`. Set its non-secret `AX_SNAPSHOTS_BUCKET` environment value
to `gs://ate-snapshots/blaxsmith/`; Substrate's local S3 backend resolves that
storage prefix. Do not disable API TLS or certificate verification.

That describes the original upstream deployment. After deploying the patched
controller below, remove its namespace RoleBinding too: it no longer looks up
provider secrets. The follow-up check confirmed it cannot get secrets even in
`ax-system`. The AX server's separate role is unaffected.

```sh
bash /opt/blaxsmith-dev/build-smoke-runner.sh /opt/blaxsmith-dev/ax \
  > /opt/blaxsmith-dev/runner-image.txt
```

The script builds the pinned AX runner into a pinned Alpine AMD64 image using
crane 0.21.7. This avoids the inaccessible upstream example image and includes
only the runner and base OS tools. It contains no coding harness or model key.

## Deploy the launch patch

The original smoke runner above remains a baseline comparison. Build the patched
controller/runner using the product files, not edits to the reference checkout:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
bash integrations/ax/build.sh /opt/blaxsmith-dev/ax /opt/blaxsmith-dev/ax-fail-closed-build
bash deploy/dev/publish-ax.sh /opt/blaxsmith-dev/ax-fail-closed-build
```

Build output directories must be new; use a new name for subsequent builds. The
publish script checks binary hashes and writes each image digest beside the
build provenance. The test images contain Alpine and the respective binary;
they are not tool-equipped worker images. The controller needs its explicit
entrypoint, so patch command and image atomically:

```sh
python3 - <<'PY'
import json, pathlib
b = pathlib.Path('/opt/blaxsmith-dev/ax-fail-closed-build')
p = json.loads((b / 'provenance.json').read_text())
patch = {'spec': {'template': {
    'metadata': {'annotations': {'blaxsmith.dev/ax-patch-sha256': p['patch_sha256']}},
    'spec': {'containers': [{'name': 'controller',
        'image': (b / 'ax-controller.image').read_text().strip(),
        'command': ['/usr/local/bin/ax-controller']}]}}}}
(b / 'controller-patch.json').write_text(json.dumps(patch))
PY
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl -n ax-system patch deployment ax-controller --type=strategic \
  --patch-file=/opt/blaxsmith-dev/ax-fail-closed-build/controller-patch.json
kubectl -n ax-system rollout status deployment ax-controller --timeout=60s
kubectl -n ax-system delete rolebinding ax-controller --ignore-not-found
python3 deploy/dev/probe-launch.py \
  "$(cat /opt/blaxsmith-dev/ax-fail-closed-build/ax-task-runner.image)" \
  /opt/blaxsmith-dev/launch-probe-evidence
```

The probe uses a unique synthetic atespace and leaves its task suspended. Its
report records missing gateway/workspace rejection before actor creation;
failure to start the command after missing input, directory, Git, skills or
required-goal setup failure; a successful command; and suspend/resume persistence.
The successful run's files are at `/opt/blaxsmith-dev/launch-probe-evidence-2`
on this node; the first invocation lacked `KUBECONFIG` for `kubectl-ate` and
stopped before creating a runner. [The report](../../docs/launch-probe.json) and
[build provenance](../../integrations/ax/provenance.json) are retained in Git.

That first egress probe reported **UNENFORCED**: the dev registry remained
reachable under an empty actor policy. It is retained as the failing baseline.

## Deploy and check the egress follow-up

The [Substrate gateway overlay](../../integrations/substrate/README.md) checks
IP/CIDR policy for every new CONNECT. The second AX overlay rejects hostnames
and ports that gateway cannot enforce and persists empty deny-all policies.
Build both from exported pinned sources on this node:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
bash integrations/substrate/build.sh /opt/blaxsmith-dev/substrate \
  /opt/blaxsmith-dev/substrate-egress-build-1
bash deploy/dev/publish-atenet.sh /opt/blaxsmith-dev/substrate-egress-build-1
bash integrations/ax/build.sh /opt/blaxsmith-dev/ax \
  /opt/blaxsmith-dev/ax-egress-build-4
bash deploy/dev/publish-ax.sh /opt/blaxsmith-dev/ax-egress-build-4
```

Build output directories must be new. The published `*.image` files contain
digest-pinned image names. Apply both images with their entrypoints and patch
hashes, then wait for both rollouts:

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
python3 - <<'PY'
import json, pathlib
root = pathlib.Path('/opt/blaxsmith-dev')
for build_name, patch_name, container, command, annotation in [
    ('substrate-egress-build-1', 'deployment-patch.json', 'ext-proc', 'atenet',
     'blaxsmith.dev/substrate-egress-patch-sha256'),
    ('ax-egress-build-4', 'controller-patch.json', 'controller', 'ax-controller',
     'blaxsmith.dev/ax-egress-patch-sha256'),
]:
    build = root / build_name
    record = json.loads((build / 'provenance.json').read_text())
    annotations = {annotation: record.get('egress_patch_sha256', record['patch_sha256'])}
    if command == 'ax-controller':
        annotations['blaxsmith.dev/ax-patch-sha256'] = record['patch_sha256']
    patch = {'spec': {'template': {
        'metadata': {'annotations': annotations},
        'spec': {'containers': [{'name': container,
            'image': (build / (command + '.image')).read_text().strip(),
            'command': ['/usr/local/bin/' + command]}]}}}}
    (build / patch_name).write_text(json.dumps(patch))
PY
kubectl -n ate-system patch deployment atenet-egress --type=strategic \
  --patch-file=/opt/blaxsmith-dev/substrate-egress-build-1/deployment-patch.json
kubectl -n ax-system patch deployment ax-controller --type=strategic \
  --patch-file=/opt/blaxsmith-dev/ax-egress-build-4/controller-patch.json
kubectl -n ate-system rollout status deployment atenet-egress --timeout=120s
kubectl -n ax-system rollout status deployment ax-controller --timeout=120s
```

The exact applied patch JSON files are retained in the remote build directories.
Both rollouts completed. Run the probe with the new runner image:

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
python3 deploy/dev/probe-launch.py \
  "$(cat /opt/blaxsmith-dev/ax-egress-build-4/ax-task-runner.image)" \
  /opt/blaxsmith-dev/launch-probe-egress-5
```

The [new report](../../docs/egress-probe.json) records one dev registry
endpoint reachable under allow-all and a matching `/32`, then blocked by an
empty and a nonmatching policy. A task without a gateway also could not reach
the endpoint. The probe repeats launch and resume checks and leaves both tasks
suspended. The live images were Substrate
`atenet@sha256:23b063647b818fd2d48534b198a5eda3bd120abc1e37a39342b1f98674a0a12b`,
AX controller
`@sha256:b07abc5372d2976a81c78b0c3da0e90ebb9fe69395f634088319b002ffcc8514`,
and AX runner
`@sha256:ea1ac8a6c3f5752957594e19754eb0adf2ca14e1238387d266e9201252a44067`.
This proves policy enforcement on those **new connections** in the tested path;
existing tunnels, alternate egress paths, tenant isolation, bootstrap, and
credential revocation remain unproved. Only synthetic tasks run here.

## Probe the synthetic bootstrap gate

Build and publish the four AX overlays, then update the AX controller image
and command as in the platform-key section below. The resulting controller must
construct gated ActorTemplates with Substrate `/healthz` transport readiness;
the runner still reports workspace readiness on `/readyz`. The tested Linux
build is [recorded here](../../integrations/ax/provenance-bootstrap.json).
The dev node has Python's `cryptography` package for the root-owned synthetic
Ed25519 signer below. Run with the router's in-cluster service IP; the probe verifies its
HTTPS certificate and uses a short-lived connector-audience token only on
bootstrap routes:

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export BLAXSMITH_DEV_SIGNING_KEY_FILE=/opt/blaxsmith-dev/platform-bootstrap-signing.key
python3 deploy/dev/probe-bootstrap.py \
  "$(cat /opt/blaxsmith-dev/ax-platform-key-build-1790118073/ax-task-runner.image)" \
  10.43.36.216 /opt/blaxsmith-dev/bootstrap-probe-evidence-4
```

Use a new evidence directory per invocation. The [passing report](../../docs/bootstrap-gate-probe.json)
records closed workspace readiness before release, successful signed release,
and a fresh challenge/replay denial after data-snapshot resume. The one-worker
pool may cold-start the task before the golden snapshot is ready; the probe
releases and suspends the cold actor, then waits for the golden snapshot before
resuming. An earlier run with Substrate pointed at `/readyz` deadlocked actor
activation. The later [actor-fence run](../../docs/bootstrap-actor-fence-gate.json)
also rejects a stale actor UID under connector-authenticated HTTPS. This is
only a synthetic startup/replay proof; there is no platform credential delivery.

The [resource-contract follow-up](../../docs/ax-resource-limits-probe.json)
uses the same signed bootstrap flow with equal AX requests and limits of 1 CPU
and 1 GiB. It leaves Task `debug` off, verifies the Substrate template values,
and reads the actual gVisor `_pause` cgroup from the worker's host cgroup tree:
`cpu.max=100000 100000`, `memory.max=1073741824`. To repeat against the current
runner build, use a new evidence directory:

```sh
python3 deploy/dev/probe-bootstrap.py \
  "$(cat /opt/blaxsmith-dev/ax-resource-contract-build-20260924-3/ax-task-runner.image)" \
  10.43.36.216 /opt/blaxsmith-dev/ax-resource-probe-next
```

Substrate currently uses the same ActorTemplate CPU/memory bound for placement
and gVisor sizing, so AX rejects unequal requests and limits. The probe reads
the configured cgroup caps; it does not test behavior under CPU or memory
pressure, and this resource class is not admin-configurable yet.

## Require the platform bootstrap key and pinned runner

Build and publish the AX overlays, including `platform-bootstrap-key.patch`.
Create the **synthetic dev** private signing key once on this node, outside the
repository; keep its file owner-only. The controller receives only its public
key and the exact runner image digest. Existing older runner images are denied
when the controller is gated.

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
built=/opt/blaxsmith-dev/ax-platform-key-build-$(date +%s)
bash integrations/ax/build.sh /opt/blaxsmith-dev/ax "$built"
bash deploy/dev/publish-ax.sh "$built"
python3 - <<'PY'
import os
path = '/opt/blaxsmith-dev/platform-bootstrap-signing.key'
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
os.write(fd, os.urandom(32))
os.close(fd)
PY
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
python3 deploy/dev/enable-platform-bootstrap.py "$built" \
  /opt/blaxsmith-dev/platform-bootstrap-signing.key
```

If the key already exists, skip the creation block. The current tested
controller and runner digests are
`127.0.0.1:5001/blaxsmith-ax-controller@sha256:984fb0875f8db1905cf289e803b16df05a30979cb3f91d2e4a33664b4c8f8052`
and
`127.0.0.1:5001/blaxsmith-ax-task-runner@sha256:40f40e9ad34c3d89c5c4c084dfe1233cf639535e0c8a666a86eb4a292d2c12e4`.
The [live probe](../../docs/bootstrap-platform-key-probe.json) confirmed
that a Task-level signer or different runner image fails before actor launch;
the valid task has no signer in its YAML and remains gated until a signed
release. This key is a dev test authority, not product custody.

## Probe activation-bound actor attestation

Build the pinned Substrate overlays from an exported source revision and
publish the verified worker binary atop the original digest-pinned worker
image. Use a new build directory each time:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
built=/opt/blaxsmith-dev/ateom-attest-build-$(date +%s)
bash integrations/substrate/build.sh /opt/blaxsmith-dev/substrate \
  "$built"
bash deploy/dev/publish-ateom.sh "$built" \
  "$(cat /opt/blaxsmith-dev/worker-image.txt)"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl -n ax-system patch workerpool blaxsmith-smoke --type=merge \
  -p "{\"spec\":{\"workerImage\":\"$(cat "$built/ateom-gvisor.image")\"}}"
```

The initial probe used the then-unauthenticated HTTP router. It resumed the
synthetic gated Task, read the current actor UID, requested a fresh
signed challenge, verified the certificate against the cluster actor CA, and
checked wrong UID/nonce/body/CA plus malformed nonce and wrong actor route. It
suspended the task in its cleanup path. The [passing report](../../docs/actor-attestation-probe.json) and
[build provenance](../../integrations/substrate/provenance-attestation.json)
record the tested result. The deployed worker image is
`127.0.0.1:5001/blaxsmith-ateom-gvisor@sha256:355c526a5c7c1aefe619d942a00670db17b3699a0e33744972573606a9e9ebcd`.
The proof and CA files in the remote evidence directory contain no secrets;
only the summary report is checked in.

That first endpoint was reachable by an unauthenticated ingress client. The
router follow-up below closes that path for bootstrap requests. Actor identity
alone is not authorization or private Git access.

## Deploy and probe connector-authenticated bootstrap ingress

Publish the newly verified `atenet` binary and configure the router's
audience, connector service-account username, and TokenReview permission:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
built=/opt/blaxsmith-dev/substrate-router-auth-build-$(date +%s)
bash integrations/substrate/build.sh /opt/blaxsmith-dev/substrate "$built"
bash deploy/dev/publish-atenet.sh "$built"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
python3 deploy/dev/enable-bootstrap-router-auth.py "$built"
python3 deploy/dev/probe-attestation.py blaxsmith-gate-2bb31a39f4 \
  gated-runner 10.43.36.216 \
  "/opt/blaxsmith-dev/router-auth-probe-evidence-$(date +%s)"
```

The probe obtains three short-lived synthetic service-account tokens in memory,
validates the router's HTTPS certificate against the service-DNS cluster trust
bundle, rejects missing/wrong-principal/wrong-audience/plaintext requests
before actor resume, then repeats actor proof verification. The plaintext case
spoofs `x-forwarded-proto: https`; the router uses Envoy's connection TLS
attribute instead. The probe suspends the Task in its cleanup path. The
[earlier passing report](../../docs/bootstrap-router-auth-probe.json) and
[Linux provenance](../../integrations/substrate/provenance-router-auth.json)
record that tested combination. The current dev router also includes the
[actor-UID fence](../../integrations/substrate/README.md), with image
`127.0.0.1:5001/blaxsmith-atenet@sha256:c814f43dc8bb46108e7a2dd5487f73a3b59b9c21b3abfe5354192773b33918f2`.
The router service account can create TokenReviews; the synthetic connector
service account cannot. The dev probe uses operator-issued TokenRequest tokens;
a product connector must use a projected audience-scoped token. No token is
written to the report or repository.

## Probe the PostgreSQL-backed synthetic connector

On the dedicated node, PostgreSQL 18 listens only on its Unix socket. The
`blaxsmith_dev` database and limited peer-authenticated `root` role hold only
the synthetic bootstrap/access schema. It applies migrations 0001-0006,
0019, 0027, and 0028 directly; it has no `blaxsmith_schema_migrations` ledger
and is not the application database. A fresh probe database must use a fresh
database and the same migration subset in order. The fourth migration adds
non-secret access-authority records, the fifth adds versioned encrypted secret
rows, and the sixth records delivery leases. Run with a new output directory
each time:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export BLAXSMITH_DEV_SIGNING_KEY_FILE=/opt/blaxsmith-dev/platform-bootstrap-signing.key
export BLAXSMITH_DEV_LEDGER=1
python3 deploy/dev/probe-bootstrap.py \
  "$(cat /opt/blaxsmith-dev/ax-exact-commit-build-1790137000/ax-task-runner.image)" \
  10.43.36.216 "/opt/blaxsmith-dev/ledger-release-probe-$(date +%s)"
```

The probe creates a fresh AX task, obtains a short-lived connector token in
memory, reads the actor and template from Substrate, and calls the Go connector.
It issues and redeems two database challenges, records release intent before
each send, rechecks the live actor, and deactivates the owner after suspension.
The [passing report](../../docs/bootstrap-ledger-release-probe.json) has no
token or signing key. The second release follows data-snapshot resume; the old
release is rejected. A newer offer also supersedes an unreleased redemption in
the local database test. The dev authorizer checks only image/pool/gVisor and
data-only snapshot settings; no
grant, effective egress check, credential, or private Git access is involved.

## Probe encrypted synthetic private Git setup

The next probe uses a root-only synthetic token and a private HTTPS Git fixture
bound to the node's `10.42.0.1` k3s bridge. Its test CA is built into the
digest-pinned runner image; the Task and Workspace contain only the clean
repository URL. Use fresh fixture, build, and evidence directories:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
fixture=/opt/blaxsmith-dev/private-git-fixture-$(date +%s)
python3 deploy/dev/private-git-fixture.py prepare "$fixture"
unit=blaxsmith-private-git-fixture
systemctl stop "$unit" 2>/dev/null || true
systemctl reset-failed "$unit" 2>/dev/null || true
systemd-run --unit="$unit" --property=RuntimeMaxSec=900 \
  /usr/bin/python3 /opt/blaxsmith-dev/blaxsmith-work/deploy/dev/private-git-fixture.py serve "$fixture"
substrate_built=/opt/blaxsmith-dev/substrate-bootstrap-phase-$(date +%s)
bash integrations/substrate/build.sh /opt/blaxsmith-dev/substrate "$substrate_built"
bash deploy/dev/publish-ateom.sh "$substrate_built" "$(cat /opt/blaxsmith-dev/worker-image.txt)"
kubectl -n ax-system patch workerpool blaxsmith-smoke --type=merge \
  -p "{\"spec\":{\"workerImage\":\"$(cat "$substrate_built/ateom-gvisor.image")\"}}"
kubectl -n ax-system rollout status deployment/blaxsmith-smoke --timeout=120s
built=/opt/blaxsmith-dev/ax-private-git-build-$(date +%s)
bash integrations/ax/build.sh /opt/blaxsmith-dev/ax "$built"
bash deploy/dev/publish-ax.sh "$built" "$fixture/ca.pem"
python3 deploy/dev/enable-platform-bootstrap.py "$built" \
  /opt/blaxsmith-dev/platform-bootstrap-signing.key
export BLAXSMITH_DEV_SIGNING_KEY_FILE=/opt/blaxsmith-dev/platform-bootstrap-signing.key
export BLAXSMITH_DEV_LEDGER=1
export BLAXSMITH_DEV_GIT_REPO=https://10.42.0.1:8443/private.git
export BLAXSMITH_DEV_GIT_COMMIT="$(git -C "$fixture/source" rev-parse HEAD)"
export BLAXSMITH_DEV_GIT_TOKEN_FILE="$fixture/token"
secret_key=/opt/blaxsmith-dev/platform-secret-key
if test ! -e "$secret_key"; then (umask 077; head -c 32 /dev/urandom > "$secret_key"); fi
export BLAXSMITH_DEV_SECRET_KEY_FILE="$secret_key"
evidence=/opt/blaxsmith-dev/private-git-probe-$(date +%s)
python3 deploy/dev/probe-bootstrap.py "$(cat "$built/ax-task-runner.image")" \
  10.43.36.216 "$evidence"
python3 deploy/dev/scan-private-git.py "$fixture/token" "$evidence" \
  > "$evidence/secret-scan.json"
systemctl stop "$unit"
```

The probe seeds synthetic provider, connection, project policy, grant, and
attempt binding rows before task launch, then encrypts the fixture token in a
versioned database row. The connector checks the live rows and reads the
current secret under the same release transaction; it does not reread the
fixture token file. The separate owner-only key file is not stored in PostgreSQL.
Each release reserves a lease with its durable release intent, then records
the secret version and acknowledged delivery in the send transaction. The
probe checks both lease rows after initial setup and resume.

The [passing live report](../../docs/bootstrap-phase-forward-probe.json) and
[bounded secret scan](../../docs/bootstrap-phase-forward-secret-scan.json) use
runner image `sha256:b7609897537db913ff8e8a9bd88b9d28868f91032e8b50b78021f34d41f025db`
and worker image `sha256:6d86f9488ec2331cc679be069874821953b878d5f525117243e2be7777528466`.
The report proves the `setup` phase through the worker's actor tunnel, the
frozen private checkout, and a fresh setup challenge after data-snapshot resume.
The companion scan found no synthetic token in evidence, AX Redis, runtime
logs, PostgreSQL, or fixture logs. The task does not request a model credential;
live `model`-phase delivery remains unproved. Generate a fresh fixture and CA
for each run because the synthetic certificate expires. The probe turns AX
`debug` on only so its test code can read back the checkout; normal product
Tasks remain debug-off.

### Synthetic post-ready model phase

This probe uses the real tool-worker and AX model gate with a fake Codex
executable. It never calls OpenAI. The model source is the immutable public
`octocat/Hello-World` commit in `probe-bootstrap.py`; the synthetic key is
encrypted in `blaxsmith_dev`, delivered only after AX reports
`WorkspaceReady=True/SetupComplete`, then checked for mode 0600 and removed by
the runner. Its Gateway allows only the pinned source host, so provider egress
is denied throughout the probe. The same sequence repeats after data-only resume. The fake runner
image has a separate registry proof and is enabled only for the probe.

On the dev node, build a fresh AX/tool-worker chain, publish both images, and
temporarily allow the probe image:

```sh
cd /opt/blaxsmith-dev/blaxsmith-work
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ax_built=/opt/blaxsmith-dev/ax-model-probe-$(date +%s)
bash integrations/ax/build.sh /opt/blaxsmith-dev/ax "$ax_built"
bash deploy/dev/publish-ax.sh "$ax_built"
signer=/opt/blaxsmith-dev/platform-bootstrap-signing.key
python3 deploy/dev/enable-platform-bootstrap.py "$ax_built" "$signer"
worker_built=/opt/blaxsmith-dev/tool-worker-model-probe-$(date +%s)
bash deploy/tool-worker/build.sh "$ax_built" "$worker_built" --buildah
worker_tag=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["local_tag"])' "$worker_built/proof.json")
bash deploy/tool-worker/publish-buildah.sh "$worker_built" \
  "127.0.0.1:5001/blaxsmith-tool-worker:${worker_tag##*:}"
probe_built=/opt/blaxsmith-dev/model-probe-image-$(date +%s)
bash deploy/dev/build-model-probe-image.sh "$worker_built" "$probe_built"
probe_tag=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["local_tag"])' "$probe_built/proof.json")
bash deploy/tool-worker/publish-buildah.sh "$probe_built" \
  "127.0.0.1:5001/blaxsmith-tool-worker:${probe_tag##*:}"
python3 deploy/dev/enable-platform-bootstrap.py "$ax_built" "$signer" "$probe_built/registry-proof.json"
```

Create a fresh owner-only test key and run the probe. Its scanner checks the
same AX, database, Redis, log, and evidence surfaces as the Git-token probe.

```sh
secret_key=/opt/blaxsmith-dev/platform-secret-key
if test ! -e "$secret_key"; then (umask 077; head -c 32 /dev/urandom > "$secret_key"); fi
model_token=/opt/blaxsmith-dev/model-probe-key-$(date +%s)
(umask 077; printf 'synthetic-model-%s' "$(openssl rand -hex 24)" > "$model_token")
export BLAXSMITH_DEV_SIGNING_KEY_FILE="$signer"
export BLAXSMITH_DEV_LEDGER=1
export BLAXSMITH_DEV_SECRET_KEY_FILE="$secret_key"
export BLAXSMITH_DEV_MODEL_TOKEN_FILE="$model_token"
probe_image=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["image"])' "$probe_built/registry-proof.json")
evidence=/opt/blaxsmith-dev/model-phase-probe-$(date +%s)
cleanup_model_probe() {
  python3 deploy/dev/enable-platform-bootstrap.py "$ax_built" "$signer"
  rm -f "$model_token"
}
trap cleanup_model_probe EXIT
python3 deploy/dev/probe-bootstrap.py "$probe_image" 10.43.36.216 "$evidence"
python3 deploy/dev/scan-private-git.py "$model_token" "$evidence" > "$evidence/secret-scan.json"
cleanup_model_probe
trap - EXIT
```

The final command restores the allowlisted runner to the pinned AX runner
image. Retain the probe build and evidence directories; never use the fake
image for product runs. A passing report proves the encrypted `model.invoke`
lease and post-ready credential handoff on AX, but it is still a synthetic
connector run, not product dispatcher/UI wiring or a real provider request.
The current rerun checks the trusted template's data-only pause/commit and
golden-image resume settings before release, pins the dev snapshot bucket,
and confirms the suspended actor's external snapshot is data-only. The
encrypted Git setup names the exact fixture commit; the runner rejects a
different fetched SHA before checkout, and the probe checks the live checkout.
The [Linux build provenance](../../integrations/ax/provenance-encrypted-git.json)
records the tested source and binary hashes. PostgreSQL recorded two completed
releases, two delivered access leases, and an inactive generation-3 owner. The test found no token in AX
Redis, inspected logs and control-plane objects, Git config, task environment,
or evidence files. The first resume attempt exposed a missing `/ax` marker;
the revised runner writes a URL-and-commit-bound marker to the snapshotted workspace and
rejects an existing `.git` without it or with a changed remote. This is a
synthetic proof only: grants, bindings, and leases are operator-seeded rather
than authorized through product APIs; there is no real provider credential,
and full-snapshot memory/complete egress/provider revocation remain open.

## Original baseline task

Replace `__WORKER_IMAGE__` and `__SUBSTRATE_VERSION__` in `workerpool.yaml` with
the digest from `worker-image.txt` and the node's `ate.dev/substrate-version`
label; replace `__RUNNER_IMAGE__` in `smoke-task.yaml` with `runner-image.txt`.
The rendered files used in the test are retained on the node as
`workerpool-rendered.yaml` and `smoke-task-rendered.yaml`.

## Smoke evidence and rechecking

The test created a one-worker gVisor pool and a synthetic AX task, read its
`blaxsmith-ax-smoke-ok` output through `ax ssh`, wrote a separate marker after
boot, suspended the task, resumed it, and read the unchanged marker. This proves
command execution, guest access, snapshot storage, and workspace-file persistence
for this version combination. It does not prove complete process restoration,
model adapters, credential revocation, or a Blaxsmith assignment/result bridge.

```sh
ax -a blaxsmith-smoke describe task blaxsmith-smoke
ax -a blaxsmith-smoke resume task blaxsmith-smoke
# Wait for Phase: Running before guest access.
ax -a blaxsmith-smoke ssh blaxsmith-smoke -- cat /workspace/result.txt
ax -a blaxsmith-smoke ssh blaxsmith-smoke -- cat /workspace/resume-proof
ax -a blaxsmith-smoke suspend task blaxsmith-smoke
```

The first submission returned `ResourceExhausted: no free workers available`
while Substrate prepared the template's golden snapshot. AX left it `Failed`
after capacity became available. One explicit resume then succeeded; subsequent
suspend/resume succeeded. The connector needs bounded retries for this transient
case, with visible reasons and an attempt budget. Do not treat it as evidence
that an engineering task or model failed. The synthetic task was left suspended.

The upstream dev overlay still uses demo object-store credentials, and upstream
AX Redis has no durable PVC. The control plane is a trusted single-user test
environment, not a production installation or tenancy boundary. Keep sensitive
work out until the [remaining runtime/access gates](../../docs/phase-0-baseline.md)
pass. Product Helm packaging, durable state/restore, authenticated connector
enrollment, secrets delivery, and multi-tenant isolation remain planned work.

## Redeploying the preview from a commit (`golive/`)

[`golive/`](golive/) holds the numbered steps used to ship a commit to the dev
node. `WORKSTATION` scripts run locally; `NODE` scripts run on the node from
`/opt/blaxsmith-dev/golive-<sha7>` with `S=<sha7>`. The usual app-only redeploy:

```sh
git bundle create /tmp/b.bundle <deployed-sha>..main
BUNDLE_REF=refs/heads/main bash deploy/dev/golive/00-local-sync.sh /tmp/b.bundle
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> BUNDLE_REF=refs/heads/main bash 01-node-sync.sh'
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> bash 06-app-image.sh'   # prints APP=...
S=<sha7> BUNDLE_REF=refs/heads/main bash deploy/dev/golive/10-local-values-apply.sh  # diff, confirm, apply
```

Runner images (`04*-workers-*.sh`) and the AX allowlist (`05-allowlist.sh`) are
only needed when `internal/tooladapter`, `cmd/blaxsmith-tool-worker`, or the
runner Dockerfile change. Take `07-db-backup.sh` before any release with new
migrations. `09-tokens.sh` re-mints the connector tokens without printing them.

### Authenticated guest router (terminals, takeover, result reads)

Upstream atenet-router routes guest daemon gRPC (`/ateenv.*`: process exec and
file read/write, exposed by AX `spec.debug=true` tasks) to whichever actor
`ate-target-actor` names, with no caller authentication on any port; the
connector TokenReview covers only `/blaxsmith/bootstrap/*` and
`/blaxsmith/command-exit`. The app now calls the guest router over the router's
HTTPS listener with TLS pinned to `router-ca.pem` and a per-RPC bearer; startup
fails if either is missing. With `dispatch.guestRouterToken.projected: true`
(the preview default) the bearer is a projected ServiceAccount token for the
chart-created `preview-app` ServiceAccount (audience `blaxsmith-bootstrap`,
1h, rotated by the kubelet and re-read per call), so it never needs
re-minting and guest access has its own identity,
`system:serviceaccount:blaxsmith-preview:preview-app`. Without it the app falls
back to the manually minted `bootstrap-token` (24h, `09-tokens.sh`). The router
side is [`guest-router-auth.patch`](../../integrations/substrate/README.md)
with `--guest-client-auth --guest-client-username=<that identity>`: TLS plus a
TokenReview for exactly that username on `/ateenv.*` (the bootstrap connector
token is refused there, and the app token is refused on bootstrap routes), and
the bearer is stripped before the guest. Order matters: an unpatched router
would forward the token into the guest.

```sh
# 1. node: build (reused if present), publish, show the diff, confirm, patch, probe
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> bash 12-guest-router-auth.sh'
# 2. workstation: app image + values (guestRouter :443, projected token, ServiceAccount)
S=<sha7> BUNDLE_REF=refs/heads/main bash deploy/dev/golive/10-local-values-apply.sh
# 3. node: rerun for the positive probe as preview-app (no router change the second time)
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> bash 12-guest-router-auth.sh'
# 4. node: narrow the router NetworkPolicy (app :443 only, ax-controller :80 only)
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> bash 08-netpol.sh'
ssh ... 'cd /opt/blaxsmith-dev/golive-<sha7> && S=<sha7> bash 11-verify.sh'
```

Between steps 1 and 2 the running app still dials plaintext `:80` and gets
426, so terminals and result reads fail until the new app rolls out. The
app identity is still all-or-nothing across guests; per-attempt access
remains the app's authorization. Operator `ax debug` through a router
port-forward is refused while the flag is on.

`13-db-app-role.sh` (once per database) moves the preview app off the Postgres
bootstrap superuser onto `blaxsmith_app` (NOSUPERUSER NOBYPASSRLS, owner of every
object), so row-level security applies. Run `07-db-backup.sh` first; the new
password goes straight into the `preview-app-db` Secret and is never printed.
