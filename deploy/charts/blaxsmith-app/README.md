# Direct-TLS application chart

This chart stages the authenticated API and built web workspace from one
`Dockerfile` `app` image. It creates a ClusterIP HTTPS Service and no ingress.
The application checks PostgreSQL on `/healthz`, serves `/livez` independently,
and applies/verifies migrations before binding. In `ha.enabled` mode, app pods
only verify that every embedded migration has already been applied; a missing,
unknown, or edited migration blocks readiness. No AX connector or agent
workers are installed by this chart.
Run creation is deliberately unavailable; no chart value can enable it before
the separate connector and durable dispatch/reconciliation loops are wired.

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

Run `deploy/charts/blaxsmith-app/test.sh` to lint and render both modes. The
app image is built in packaging CI. This is a staging foundation, not a
real-user production installation: no external L4 exposure or trusted client
address design, key-rotation rollout, RBAC-protected project APIs, or AX
connector is packaged yet. The plaintext public preview remains a separate
chart; do not expose it as the authenticated product.
