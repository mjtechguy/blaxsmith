# AX Redis connection compatibility

The [pinned overlay](../integrations/ax/redis-ha.patch) gives the AX server and
controller the same Redis connection settings. It applies after the
[task-tombstone overlay](ax-task-tombstones.md) in the supported AX build at
`f009cc81c9a571073bc1dd58cd2ed934bf2d5b1c`. The dev node keeps the local
single Redis address with no TLS; HA Redis is not enabled there.

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
an authenticated Sentinel. They wait until a task tombstone and a pending stream
entry appear on the replica, promote it, verify the client reconnects through
Sentinel, recover the pending entry, and verify a late `SaveTask` remains rejected.
Another test uses a real TLS Redis process
with a locally generated certificate; an incorrect server name is rejected.
The pinned [build script](../integrations/ax/build.sh) runs these tests and
records the overlay hash in provenance.

The [consumer-recovery overlay](../integrations/ax/consumer-recovery.patch)
adds `XAUTOCLAIM` after one minute of pending idle time. The controller reads
one event at a time and renews its ownership every ten seconds while it
reconciles. Every controller process gets a random consumer-ID suffix, including
when `--redis-consumer` supplies a readable prefix. Renewal failure cancels
reconciliation and leaves the entry pending.
Acknowledgement checks ownership atomically, so a former consumer cannot erase
an entry another controller claimed. A real Redis restart test verifies the
pending entry survives, is claimed by a replacement, and rejects the old
consumer's acknowledgement. Redis 6.2 or newer is required for `XAUTOCLAIM`.

**This is connection compatibility, not yet a production HA claim.** Redis
replication is asynchronous. A tombstone acknowledged by the old master can be
lost if promotion occurs before it reaches the chosen replica; the test
deliberately waits for replication and does not close that window. A production
topology needs a proven durable tombstone/fencing contract across all allowed
promotions, persistent no-eviction Redis storage and restore drills, and a
multi-Sentinel or managed failover test under real outage conditions. Stream
recovery remains at-least-once: if a controller loses Redis while an external
Substrate call completes despite context cancellation, its replacement can
repeat that call. The product must prove per-attempt Substrate side effects are
idempotent or externally fenced before enabling multiple controllers.
Do not enable automatic uncertain-attempt cancellation or call the AX execution
plane HA until these gates pass. PostgreSQL remains the authoritative product
workflow ledger.
