# Direct-TLS application chart

This chart stages the authenticated API and built web workspace from one
`Dockerfile` `app` image. It creates a ClusterIP HTTPS Service and no ingress.
The application checks PostgreSQL on `/healthz`, serves `/livez` independently,
and applies/verifies migrations before binding. In `ha.enabled` mode, app pods
only verify that every embedded migration has already been applied; a missing,
unknown, or edited migration blocks readiness. AX dispatch is disabled by
default. When enabled, the chart copies pinned AX and Substrate CLIs from a
separate tools image, projects dispatch credentials, and adds a loopback AX
tunnel sidecar. It does not grant the app pod Kubernetes API access.

Supply an immutable `repository@sha256:<manifest digest>` app image and three
existing namespace Secrets: a `kubernetes.io/tls` Secret with `tls.crt` and
`tls.key`; a signer Secret with a raw 32-byte Ed25519 seed under `seed`; and a
database Secret with a PostgreSQL URL under `url`. The certificate must match
the exact `origin` value. PostgreSQL requires verified TLS by default. The
chart projects TLS and signer material as group-readable, non-world-readable
files for UID/GID 65532; it does not create, log, or rotate those secrets.
For a PostgreSQL server certificate signed by a private CA, set
`databaseCASecretName` to an existing namespace Secret containing `ca.crt`, and
put `sslmode=verify-full&sslrootcert=/run/blaxsmith/database-ca/ca.crt` in the
database URL Secret. The URL host must match the PostgreSQL server certificate
(for CNPG, typically `<cluster>-rw.<namespace>.svc`). A missing CA key blocks
pod startup; an untrusted or mismatched certificate blocks database readiness.
For a planned signing-key rotation, set `previousSignerPublicSecretName` to an
existing Secret containing a raw 32-byte key under `public`. Pre-stage the new
public key while the old signer is active, roll to the new signer while trusting
the old public key, then remove that trust after the access-token lifetime and
rollout/clock margin. Each step requires a rollout; use the same Secret versions
across replicas.

## Optional AX dispatch

The chart keeps dispatch off unless `dispatch.enabled=true`. Dispatch needs an
immutable `dispatch.tools.image` containing `/usr/local/bin/ax`,
`/usr/local/bin/kubectl-ate`, and `socat` in `PATH`. An init container copies
the two CLIs into a pod-local `emptyDir`; the app mounts that directory
read-only at `/opt/blaxsmith/dispatch-tools`. Their lowercase SHA-256 values
are checked by `serve-app` at startup. This lets the current app image run
without rebuilding it to add AX/Substrate tools. The app image also needs the
existing 32-byte `accessKeySecretName` Secret for encrypted credential
storage.

On the dev node, the existing setup instructions build `/usr/local/bin/ax`
and `/usr/local/bin/kubectl-ate`. Copy those binaries into an image build
context and make an immutable tools image, for example:

```dockerfile
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache socat
COPY --chmod=0555 ax /usr/local/bin/ax
COPY --chmod=0555 kubectl-ate /usr/local/bin/kubectl-ate
```

```sh
mkdir -p /opt/blaxsmith-dev/dispatch-tools
cp /usr/local/bin/ax /opt/blaxsmith-dev/dispatch-tools/ax
cp /usr/local/bin/kubectl-ate /opt/blaxsmith-dev/dispatch-tools/kubectl-ate
cd /opt/blaxsmith-dev/dispatch-tools
cat > Dockerfile <<'EOF'
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache socat
COPY --chmod=0555 ax /usr/local/bin/ax
COPY --chmod=0555 kubectl-ate /usr/local/bin/kubectl-ate
EOF
buildah bud --platform linux/amd64 -t 127.0.0.1:5001/blaxsmith-dispatch-tools:dev .
buildah push --tls-verify=false \
  --digestfile /opt/blaxsmith-dev/dispatch-tools-digest.txt \
  127.0.0.1:5001/blaxsmith-dispatch-tools:dev \
  docker://127.0.0.1:5001/blaxsmith-dispatch-tools:dev
```

Use the published registry reference ending in the digest from
`/opt/blaxsmith-dev/dispatch-tools-digest.txt` as `dispatch.tools.image`. Set
`dispatch.ax.cliSHA256` and `dispatch.substrate.cliSHA256` to `sha256sum` of
the exact input binaries.

Create `dispatch.credentialsSecretName` separately with these exact keys:

| Key | Contents |
| --- | --- |
| `substrate-token` | Substrate API token |
| `substrate-ca.pem` | PEM CA bundle for the Substrate API |
| `router-ca.pem` | PEM CA bundle for the HTTPS bootstrap router |
| `actor-ca.pem` | PEM CA bundle for AX actor attestation |
| `bootstrap-token` | Short-lived bootstrap-router token |
| `bootstrap-signer` | Raw 32-byte Ed25519 seed |

The chart projects these as read-only mode `0440` files for UID/GID 65532.
The same namespace must contain the configured access-key Secret with raw
32-byte value under `key`. None of these values belongs in Helm values or
command-line `--set` arguments.

The dispatch pod runs `socat` from the same tools image. It listens on the
configured loopback port (18443 by default) in the shared pod network namespace
and forwards to the configured AX service, defaulting to
`ax-server.ax-system.svc.cluster.local:8080` from the AX chart. Supply an
immutable tools image containing `socat`; no ServiceAccount token or
Kubernetes port-forward permission is needed. Both the AX service and the
Substrate/router endpoints must be reachable from the app namespace.

`dispatch.guestRouter` (terminals, takeover, guest result reads) requires
dispatch: the app calls the router's HTTPS listener with TLS pinned to
`router-ca.pem` and a bearer token on every RPC, never plaintext. The token is
`bootstrap-token` unless `dispatch.guestRouterToken.projected=true`, which
creates the `<release>-app` ServiceAccount (no RBAC) and mounts a
kubelet-rotated projected token for it (`audience`, `expirationSeconds`)
instead, so nothing needs re-minting. The router must run the
`guest-router-auth.patch` build with `--guest-client-auth` and, for the
projected token, `--guest-client-username=system:serviceaccount:<namespace>:<release>-app`;
it checks and strips the token before the guest. See `deploy/dev/README.md`.

Example values (replace all example digests and IDs with the pinned values
for the target dev cluster):

```yaml
accessKeySecretName: app-access-key
dispatch:
  enabled: true
  egressMode: exact
  credentialsSecretName: app-dispatch-runtime
  tools:
    image: REGISTRY/blaxsmith-dispatch-tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  tunnel:
    targetHost: ax-server.ax-system.svc.cluster.local
    targetPort: 8080
    localPort: 18443
  ax:
    cliSHA256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  substrate:
    cliSHA256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    endpoint: api.ate-system.svc:443
  bootstrap:
    routerURL: https://router.ate-system.svc:443
  clusterID: dev-cluster
  leaseTTL: 10m
  workerImage: REGISTRY/blaxsmith-tool-worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  workerPool: blaxsmith-pool
  snapshotStorage: blaxsmith-snapshots
  workspace: blaxsmith-workspaces
  gateway: blaxsmith-egress
  guestRouter: router.ate-system.svc:443 # optional HTTPS host:port; empty = terminals 503
```

The normal setting `egressMode: exact` retains the attempt-scoped host
allowlist. For a development cluster that must reach arbitrary model and Git
endpoints, use `egressMode: open-dev` and explicitly set
`dev.allowOpenEgress: true`; the chart then passes both the `open-dev` env
value and `--allow-open-egress-dev` flag. The app rejects open mode if either
part is absent. This creates an AX Gateway with `host: "*"`; treat it as a
development-only configuration, and leave this switch false for production.

The tunnel forwards unencrypted AX HTTP inside the cluster network because
the pinned AX CLI client only supports loopback HTTP. Use a trusted cluster
network; do not expose the tunnel port through a Service or ingress.

```sh
helm upgrade --install app deploy/charts/blaxsmith-app \
  --namespace blaxsmith --create-namespace \
  --set-string image=REGISTRY/blaxsmith-app@sha256:IMAGE_DIGEST \
  --set-string origin=https://app.example.com \
  --set-string tlsSecretName=app-tls \
  --set-string signerSecretName=app-signer \
  --set-string databaseSecretName=app-database \
  --wait
```

The default remains one replica. To stage multiple application replicas, use
Kubernetes 1.30+ with at least two schedulable nodes labeled
`kubernetes.io/hostname` and the same
database, TLS, and signer Secrets on every pod:

```sh
helm upgrade --install app deploy/charts/blaxsmith-app \
  --namespace blaxsmith --create-namespace \
  --set-string image=REGISTRY/blaxsmith-app@sha256:IMAGE_DIGEST \
  --set-string origin=https://app.example.com \
  --set-string tlsSecretName=app-tls \
  --set-string signerSecretName=app-signer \
  --set-string databaseSecretName=app-database \
  --set ha.enabled=true --set replicaCount=3 --wait
```

More than one replica requires `ha.enabled=true`; HA mode requires at least two
replicas. It spreads pods across hostnames with `maxSkew: 1` and
`minDomains: 2`; a second replica stays pending on a one-node cluster. A
disruption budget allows one voluntary eviction at a time. The
Deployment waits five seconds after readiness. Pod/node replacements preserve
replicas when capacity permits. A template change uses `Recreate`, stopping all
old app pods before starting a new version. Readiness checks PostgreSQL; liveness is independent of
it. A node failure can still reduce capacity, and a disruption budget only
governs voluntary evictions ([Kubernetes PDB semantics](https://kubernetes.io/docs/tasks/run-application/configure-pdb/)).

For an HA-mode schema upgrade, use a maintenance window and a values file for
the target release. First verify a restorable backup and quiesce **every**
Blaxsmith database writer, including any separately deployed scheduler,
bridge, or maintenance process. Stop the app Deployment and wait until its
pods are gone. Render only the one-off Job template with the **new immutable
image**, using the same database Secret and optional CA Secret. The Job requires
verified PostgreSQL TLS and applies migrations under the database advisory
lock. Check completion before changing the app release:

```sh
IMAGE=REGISTRY/blaxsmith-app@sha256:NEW_IMAGE_DIGEST
kubectl -n blaxsmith scale deployment/app-app --replicas=0
kubectl -n blaxsmith wait --for=delete pod \
  -l 'app.kubernetes.io/name=blaxsmith-app,app.kubernetes.io/instance=app' --timeout=5m
helm template app deploy/charts/blaxsmith-app --namespace blaxsmith \
  -f production-values.yaml --set-string image="$IMAGE" \
  --set-string migrationJob.name=app-migrate-20260923 \
  --show-only templates/migration-job.yaml |
  kubectl -n blaxsmith apply -f -
kubectl -n blaxsmith wait --for=condition=complete job/app-migrate-20260923 --timeout=5m
helm upgrade app deploy/charts/blaxsmith-app --namespace blaxsmith \
  -f production-values.yaml --set-string image="$IMAGE" --wait=false
kubectl -n blaxsmith scale deployment/app-app --replicas=3
kubectl -n blaxsmith rollout status deployment/app-app --timeout=5m
```

Use a unique Job name for each attempt; leave `migrationJob.name` empty in
the values file and on the Helm upgrade, so the Job is never installed as a
concurrent chart resource. Keep writers stopped and inspect the Job logs if
it fails. Do not automatically roll back to the old image after a successful
schema migration: old binaries reject migrations they do not know. This
procedure does not enforce quiescence outside the app Deployment or prove
mixed-version compatibility.

This is application replica placement, not a product HA claim. CloudNativePG,
the Barman Cloud plugin, and AX Redis remain separately operated per
[ADR 0002](../../../docs/adr/0002-ha-and-live-workspace.md). Production rollout
requires restore and failover tests plus operation reconciliation after a
scheduled migration; mixed-version schema compatibility has not been established.

Run `deploy/charts/blaxsmith-app/test.sh` to lint and render the default and
dispatch-enabled modes. The app image is built in packaging CI. This is a
staging foundation, not a real-user production installation: external L4
exposure and trusted client address design, key-rotation rollout, and
RBAC-protected project APIs remain deployment work. The chart does not build
or publish the app/CLI/tunnel images or create dispatch credentials. The
plaintext public preview remains a separate chart; do not expose it as the
authenticated product.

## Optional model gateway

`gateway.enabled=true` adds a `<release>-gw` Deployment running the same image
as `blaxsmith gateway` (docs/model-gateway-plan.md), a Service on port 8443 and,
by default, a NetworkPolicy that admits only the gateway port and allows egress
to DNS, PostgreSQL and public HTTPS (provider APIs). It needs
`accessKeySecretName` (route credentials are decrypted in the gateway) and
`gateway.publicURL`, which the app receives as `BLAXSMITH_GATEWAY_URL` and
writes into brokered attempts' harness config. Under exact AX egress the public
URL must resolve to public IPv4 addresses; put an ingress or LoadBalancer in
front of the Service and narrow `gateway.networkPolicy.ingressFrom` to it. Set
`gateway.tlsSecretName` to serve HTTPS directly; without it the gateway serves
plain HTTP for a TLS-terminating proxy. The app runs migrations; gateway pods
only verify them. On shutdown in-flight streams get `drainTimeoutSeconds`.
With the value off, no gateway exists and organizations cannot enable it.
