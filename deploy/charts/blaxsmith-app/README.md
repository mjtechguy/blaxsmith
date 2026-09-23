# Direct-TLS application chart

This chart stages the authenticated API and built web workspace from one
`Dockerfile` `app` image. It creates a ClusterIP HTTPS Service and no ingress.
The application checks PostgreSQL on `/healthz`, serves `/livez` independently,
and applies/verifies migrations before binding. No AX connector or agent
workers are installed by this chart.

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
old writers before starting a new version; plan a maintenance window rather
than expecting an unproven rolling schema upgrade. Readiness checks PostgreSQL; liveness is independent of
it. A node failure can still reduce capacity, and a disruption budget only
governs voluntary evictions ([Kubernetes PDB semantics](https://kubernetes.io/docs/tasks/run-application/configure-pdb/)).

This is application replica placement, not a product HA claim. CloudNativePG,
the Barman Cloud plugin, and AX Redis remain separately operated per
[ADR 0002](../../../docs/adr/0002-ha-and-live-workspace.md). Production rollout
requires restore and failover tests; application version upgrades still require
a scheduled migration and operation reconciliation, since mixed-version schema
compatibility has not been established.

Run `deploy/charts/blaxsmith-app/test.sh` to lint and render both modes. The
app image is built in packaging CI. This is a staging foundation, not a
real-user production installation: no external L4 exposure or trusted client
address design, key-rotation rollout, RBAC-protected project APIs, or AX
connector is packaged yet. The plaintext public preview remains a separate
chart; do not expose it as the authenticated product.
