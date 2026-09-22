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
- AX commit `d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`. The source checkout
  remains clean; the deployed controller and follow-up runner include the
  [Blaxsmith launch patch](../../integrations/ax/README.md) applied to an export.
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
cd /opt/blaxsmith-dev/blaxsmith
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

The egress probe separately reports **UNENFORCED**: the dev registry was reachable
both with allow-all and with an empty actor egress policy. This is a failed
security gate, not a passing networking test. AX's `PoliciesApplied` status proves
only storage at this revision. Keep credentials and untrusted tasks out until
destination enforcement and authenticated bootstrap are implemented and tested.

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
