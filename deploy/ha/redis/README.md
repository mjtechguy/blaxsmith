# AX Redis/Sentinel chart candidate

This is a **render-only qualification candidate**, not an installable or
supported HA profile. AX keeps task state, stream entries, and tombstones in
Redis; Redis is not a disposable cache. The existing [AX connection overlay](../../../docs/ax-redis-ha.md)
supports authenticated TLS and Sentinel discovery, but asynchronous promotion
can lose an acknowledged tombstone. Do not deploy this candidate over the
single-node development Redis or advertise production HA from a Helm render.

`values.candidate.yaml` targets the independently operated [Bitnami Redis
chart](https://github.com/bitnami/charts/blob/main/bitnami/redis/README.md),
version `23.1.1`, OCI manifest digest
`sha256:f4a368f7a67f4f2bedee2426bfb063b960565ee38a91fdf07185a014c9e63406`
as observed on 2026-09-23. `test.sh` downloads that version, checks the
manifest digest, lints, and checks the rendered configuration. The candidate
uses invalid zero image digests and missing `*-replace` Secrets/storage class,
so it fails to start until a site qualifies and supplies those dependencies.
The chart's current defaults use `latest` images; Bitnami says versioned
production images and support are in its [Secure Images
offering](https://github.com/bitnami/charts/issues/35164). Verify procurement,
registry access, image provenance, and immutable Redis/Sentinel digests before
considering this chart. Do not use unsupported `bitnamilegacy` images as a
production escape hatch.

Run the shape check with `deploy/ha/redis/test.sh`. The chart is not a
dependency of the Blaxsmith app chart. A managed Redis service can satisfy the
same AX endpoint, TLS, persistence, and recovery contract without this chart.

The candidate renders one StatefulSet of **three** Redis/Sentinel pods. In
Sentinel mode `replica.replicaCount` is the total pod count, not a count in
addition to a master. `minDomains: 3` and required scheduling spread keep a
pod Pending until three independent hostname domains are available. Confirm
the CSI volumes have independent failure domains; three PVCs on one host or
array do not meet the failure model. Redis AOF and snapshots, `noeviction`,
`stop-writes-on-bgsave-error`, and a one-replica/5-second write gate are
rendered. [Redis documents](https://redis.io/docs/latest/operate/oss_and_stack/management/replication/)
that replication and `min-replicas-to-write` cannot guarantee retention of
acknowledged writes on failover; AOF `everysec` can also lose recent local
writes on a crash ([persistence documentation](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)).
The chart's Sentinel PVC support is marked experimental upstream and needs a
simultaneous-restart drill.

Precreate the Redis password Secret with key `redis-password` and a TLS Secret
with `tls.crt`, `tls.key`, and `ca.crt`. Issue a **separate client certificate**
for each AX workload; never mount the Redis server private key into AX. The
server certificate must verify both the Sentinel seed Service name and every
Redis/Sentinel pod hostname returned by discovery. The chart announces
hostnames, but the actual DNS/SAN chain must pass an AX connection and
promotion test with `REDIS_TLS_SERVER_NAME` unset. Both Redis and Sentinel
plaintext listeners must remain disabled. Redis and Sentinel use the **same**
chart password here; distinct service credentials or scoped AX ACL users are
unproven with this chart and are a security qualification gate.

For AX in the same namespace, label only the AX API/controller pods
`ax-redis-client=true`; for another namespace, label that namespace
`blaxsmith.io/redis-client-namespace=true` and its AX pods
`app.kubernetes.io/part-of=ax`. Validate the rendered NetworkPolicy on the
chosen CNI, including DNS TCP fallback, Redis/Sentinel peer traffic, and denial
from unrelated pods. Mount client credentials from existing Secrets and pass
the following settings to **both** AX API and controller:

| AX setting | Candidate value/source |
| --- | --- |
| `REDIS_SENTINEL_MASTER` | `axmaster` |
| `REDIS_SENTINEL_ADDRS` | `ax-redis.ax-system.svc.cluster.local:26379` (the chart's Sentinel seed Service, if installed in `ax-system`) |
| `REDIS_PASSWORD`, `REDIS_SENTINEL_PASSWORD` | Secret key `redis-password`; same password is a candidate limitation |
| `REDIS_TLS` | `true` |
| `REDIS_TLS_CA_FILE` | Mounted public `ca.crt` |
| `REDIS_TLS_CERT_FILE`, `REDIS_TLS_KEY_FILE` | Mounted AX client certificate and key |

Qualification requires a supported chart/image pair, immutable digests,
three hosts and durable CSI storage, live TLS/ACL/NetworkPolicy checks,
multi-Sentinel promotion and restart tests, and an independent backup/restore
procedure for Redis data and Sentinel state. Rehearse restore **into a new
release**, including AX streams, tombstones, credentials, and reconciliation
against PostgreSQL/Substrate before allowing dispatch. Inject leader/node and
storage failures while work is active; measure acknowledged-write loss,
at-least-once replay, Redis recovery time, and the product's restricted
recovery behavior. `WAIT` or the one-replica write gate alone cannot close the
tombstone loss window. Until a durable fencing/ownership proof closes it, do
not enable automatic uncertain-attempt cancellation or multiple AX
controllers, and do not count this as product HA. See
[ADR 0002](../../../docs/adr/0002-ha-and-live-workspace.md) for the full gate.
