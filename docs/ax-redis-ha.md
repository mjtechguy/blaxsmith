# AX Redis connection compatibility

The [pinned overlay](../integrations/ax/redis-ha.patch) gives the AX server and
controller the same Redis connection settings. It applies after the
[task-tombstone overlay](ax-task-tombstones.md) at AX commit
`d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`. The local development default
remains one Redis address with no TLS. This patch has not been deployed to the
shared k3s node.

| Setting | Purpose |
| --- | --- |
| `REDIS_ADDR`, `REDIS_PASSWORD` | Existing direct-address and password settings. |
| `REDIS_USERNAME` | Redis ACL username, if used. |
| `REDIS_SENTINEL_MASTER` and `REDIS_SENTINEL_ADDRS` | Enable `go-redis` failover discovery. Both are required; addresses are comma-separated `host:port` seeds. `REDIS_ADDR` is ignored in this mode. |
| `REDIS_SENTINEL_USERNAME`, `REDIS_SENTINEL_PASSWORD` | Separate Sentinel ACL credentials. |
| `REDIS_TLS=true` | Verify TLS certificates on **both** Redis and Sentinel connections. |
| `REDIS_TLS_CA_FILE` | Additional trusted CA PEM; system roots remain trusted. |
| `REDIS_TLS_CERT_FILE`, `REDIS_TLS_KEY_FILE` | Optional client certificate/key pair. Both are required together. |
| `REDIS_TLS_SERVER_NAME` | Optional name override, only if every Redis and Sentinel endpoint presents a certificate for that same name. Usually leave unset so each address is verified by its own host. |

Supply credentials and CA/client key files from mounted Kubernetes Secrets or an
external secret controller. Do not place their contents in AX Task fields, image
arguments, ConfigMaps, or the repository. For production, enable TLS and distinct
Redis/Sentinel authentication. Mispaired Sentinel settings or TLS files fail
startup; certificate verification cannot be disabled through this overlay.

The integration tests start authenticated Redis master and replica processes and
an authenticated Sentinel. They wait until a task tombstone appears on the
replica, promote it, verify the client reconnects through Sentinel, and verify a
late `SaveTask` remains rejected. Another test uses a real TLS Redis process
with a locally generated certificate; an incorrect server name is rejected.
The pinned [build script](../integrations/ax/build.sh) runs these tests and
records the overlay hash in provenance.

**This is connection compatibility, not yet a production HA claim.** Redis
replication is asynchronous. A tombstone acknowledged by the old master can be
lost if promotion occurs before it reaches the chosen replica; the test
deliberately waits for replication and does not close that window. A production
topology needs a proven durable tombstone/fencing contract across all allowed
promotions, persistent no-eviction Redis storage and restore drills, and a
multi-Sentinel or managed failover test under real outage conditions. AX's
current stream consumer does not claim pending messages left by a dead
controller; that recovery path also needs a test and fix before controller HA.
Do not enable automatic uncertain-attempt cancellation or call the AX execution
plane HA until these gates pass. PostgreSQL remains the authoritative product
workflow ledger.
