# Optional CloudNativePG data profile

This chart renders one PostgreSQL `Cluster`, one Barman Cloud CNPG-I
`ObjectStore`, and an immediate, scheduled physical base backup. WAL archiving
starts with the Cluster. It does **not** install CloudNativePG, the Barman
plugin, cert-manager, object storage, or the Blaxsmith app. Those have separate
lifecycles; this is a qualification candidate, not a certified HA claim.

The chart has no deployable defaults. Supply a site-specific values file kept
outside source control:

```yaml
postgresImage: ghcr.io/cloudnative-pg/postgresql@sha256:REPLACE_WITH_VERIFIED_DIGEST
storageClass: REPLACE_WITH_DURABLE_CSI_CLASS
storageSize: 100Gi
topologyKey: kubernetes.io/hostname
appSecretName: blaxsmith-db-app
serverTLSSecretName: blaxsmith-db-tls
serverCASecretName: blaxsmith-db-ca
backup:
  destinationPath: s3://REPLACE_WITH_INDEPENDENT_BUCKET/blaxsmith/production
  credentialsSecretName: blaxsmith-db-backup
  endpointURL: "" # Set to HTTPS for a compatible non-AWS endpoint.
  endpointCASecretName: "" # Set if that endpoint uses a private CA.
  retentionPolicy: 30d
  schedule: "0 0 2 * * *" # Six fields: every day at 02:00:00 UTC.
monitoring:
  podMonitor: false # Enable only after installing Prometheus Operator.
```

Before installing, choose and record **tested, mutually compatible, immutable**
CloudNativePG operator, Barman plugin, and PostgreSQL image versions. The
[plugin installation guide](https://cloudnative-pg.io/plugin-barman-cloud/docs/installation/)
requires CNPG 1.26+ and cert-manager, and requires the plugin in the operator's
namespace. Pin their Helm chart versions or manifest digests in deployment
automation; never use a floating chart or `latest` image. Confirm the operator
watches the Blaxsmith namespace. Installation uses the official
[CNPG operator chart](https://cloudnative-pg.io/charts/) and
[Barman plugin chart](https://cloudnative-pg.io/plugin-barman-cloud/docs/installation/),
not this chart's dependency tree. No version pair is certified here yet.
Record the installed chart versions and image digests in the qualification
report, then check the live CRDs before rendering/applying this profile:

```sh
helm -n cnpg-system list
kubectl -n cnpg-system get deploy -o wide
kubectl get crd clusters.postgresql.cnpg.io scheduledbackups.postgresql.cnpg.io \
  objectstores.barmancloud.cnpg.io
kubectl explain cluster.spec.plugins --api-version=postgresql.cnpg.io/v1
kubectl explain objectstore.spec.configuration --api-version=barmancloud.cnpg.io/v1
```

`helm template` validates shape but cannot prove live CRD compatibility or
operator/plugin behavior. Server-side dry-run below is mandatory after the
chosen pair is installed. If a plugin or operator upgrade changes a CRD, rerun
dry-run, backup, WAL, and new-cluster restore tests before rollout.

Precreate these Secrets in the **database namespace**:

| Name from values | Required data | Custody |
| --- | --- | --- |
| `appSecretName` | `kubernetes.io/basic-auth`, `username=blaxsmith`, strong `password` | Secret manager; same credential used in app URL Secret |
| `serverTLSSecretName` | `kubernetes.io/tls` with `tls.crt`, `tls.key`; SAN includes `<release>-rw.<namespace>.svc` | Certificate issuer; rotate before expiry |
| `serverCASecretName` | `ca.crt` that verifies the server certificate | Certificate issuer; copy only CA public bytes into app namespace |
| `backup.credentialsSecretName` | `ACCESS_KEY_ID`, `ACCESS_SECRET_KEY` | Scoped object-store identity; no credentials in values |
| `backup.endpointCASecretName`, when set | `ca.crt` | Private S3 endpoint CA |

The backup destination must be an independent bucket/prefix with TLS, server-side
encryption, versioning, access controls, and a documented deletion/retention
policy. The profile deliberately references credentials instead of creating
them. For EKS, the upstream plugin also supports workload identity; use a
site-specific `ObjectStore` if replacing static credentials. Coordinate the
plugin recovery window with object lock/lifecycle so WAL needed by the oldest
retained base backup survives. A 30-day example is not a platform default.

```sh
bash deploy/charts/blaxsmith-cnpg/test.sh
helm lint deploy/charts/blaxsmith-cnpg -f /secure/path/cnpg-values.yaml
helm template blaxsmith-db deploy/charts/blaxsmith-cnpg \
  --namespace blaxsmith -f /secure/path/cnpg-values.yaml > /secure/path/cnpg-rendered.yaml
kubectl -n blaxsmith apply --dry-run=server -f /secure/path/cnpg-rendered.yaml
# After operator/plugin/Secret/storage/object-store preflight and change approval:
helm upgrade --install blaxsmith-db deploy/charts/blaxsmith-cnpg \
  --namespace blaxsmith -f /secure/path/cnpg-values.yaml
kubectl -n blaxsmith wait cluster/blaxsmith-db --for=condition=Ready --timeout=20m
```

Three instances use required anti-affinity across `topologyKey`. Verify three
distinct schedulable nodes and independent storage failure domains; a missing
domain leaves a pod Pending. A zone topology key requires three declared zones
and enough capacity. The shared single-node k3s `local-path` StorageClass is
for development only and cannot qualify this profile. The chart does not set synchronous replication or failover
quorum: select and measure that durability/availability trade-off under the
[CNPG failover rules](https://cloudnative-pg.io/docs/1.28/failover/). Do not
claim zero acknowledged-write loss from three pods alone.

The app chart reads a separately provisioned URL Secret and optional public
database CA Secret. Its URL should target
`blaxsmith-db-rw.blaxsmith.svc:5432/blaxsmith` with `sslmode=verify-full` and
`sslrootcert=/run/blaxsmith/database-ca/ca.crt`; set
`databaseCASecretName` on the app chart. Match the URL host to a server
certificate SAN. Keep the password URL-encoded in the Secret, never in Helm
values. CNPG creates the `-rw` service; do not connect the app to a specific pod.

## Backup monitoring and restore gate

Enable `monitoring.podMonitor` only if Prometheus Operator watches this
namespace and can read the referenced CA. The exporter uses TLS with the
database certificate. Alert on an absent scrape, failed/latest available base
backup timestamps, archive-ready WAL backlog, volume capacity, instance/node
count, replica lag, and failed object-store access. The plugin exports
`barman_cloud_cloudnative_pg_io_last_available_backup_timestamp`,
`barman_cloud_cloudnative_pg_io_last_failed_backup_timestamp`, and
`barman_cloud_cloudnative_pg_io_first_recoverability_point`; CNPG exports
`cnpg_collector_pg_wal_archive_status` and `cnpg_collector_nodes_used`.
Calibrate thresholds against the chosen backup schedule and measured RPO; do
not treat a green backup object as proof that a point can be restored.

After the first immediate backup completes, confirm its `phase=completed`,
that the archive contains later WAL, and that the object-store identity can
read the backup. Rehearse this [PITR procedure](../../ha/cnpg/README.md)
into a **new** cluster before enabling an HA label or SLO. A database restore
does not recover artifact bytes, signer/encryption keys, AX Redis state, or
provider revocations.
