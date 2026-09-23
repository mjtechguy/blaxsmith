# PostgreSQL PITR rehearsal

Run on a staging or approved isolated recovery namespace. Record operator,
Barman plugin, PostgreSQL image digest, Kubernetes, CSI, object-store version,
source cluster name, base backup ID, target time, and start/end times. Follow
the [CNPG recovery API](https://cloudnative-pg.io/docs/1.28/recovery/) and
[Barman plugin recovery wiring](https://cloudnative-pg.io/plugin-barman-cloud/docs/usage/#restoring-a-cluster).

1. Confirm a completed base backup before the target and WAL through it:
   `kubectl -n blaxsmith get scheduledbackup,backup,cluster`. Inspect plugin
   backup metrics, WAL archive backlog, and object-store catalog. In staging,
   commit an identifiable test row, note its UTC commit time, then commit a
   second row to be excluded. Pick an RFC3339 target between them. The target
   must be before the last archived WAL and after the chosen base backup.
2. Create a fresh namespace and a **read-only** object-store credential Secret
   there; keep it separate from the production writer credential. Copy
   `restore.yaml.example` to a private file and replace every
   `REPLACE_WITH_*` value, including the source `serverName`, target time,
   matching PostgreSQL major/image digest, and durable storage settings. Add
   `endpointURL` and `endpointCA` to its `ObjectStore` when the source used a
   private HTTPS S3 endpoint. Do not put credentials in the YAML.
3. Require `rg 'REPLACE_WITH_' /secure/path/restore.yaml` to return no matches.
   Then run `kubectl -n <rehearsal-namespace> apply --dry-run=server -f
   /secure/path/restore.yaml` and apply the same file. Wait for
   `kubectl -n <rehearsal-namespace> wait cluster/blaxsmith-restore
   --for=condition=Ready --timeout=2h`. The source production cluster is not
   changed. A restored cluster must use a **new** name and volume set.
4. Inspect the restored database with an authorized temporary client. Prove
   that the pre-target row exists and the post-target row does not. Check
   Blaxsmith's schema ledger (`SELECT version,sha256 FROM
   blaxsmith_schema_migrations ORDER BY version`) against the release binary's
   expected migrations. Inspect the recovered `access_grants` versions and
   `revoked_at`, `identity_sessions` expiry/revocation, and active
   `workflow_attempts`/`workflow_tasks` against the recorded source snapshot
   and **current external authority**. A lost revocation or an apparently
   running attempt must stay fenced; the restored database alone cannot
   establish current permission or AX ownership. Record the selected recovery
   timestamp, last replayed transaction/LSN, backup/WAL object IDs, schema
   checksum evidence, authority deltas, and elapsed time. Destroy the
   rehearsal namespace only after retaining the evidence and verifying that
   no production source objects or Secrets were deleted.
5. For a product recovery, keep app access and dispatch blocked under the
   plan's restricted-recovery authority fence. Restore and verify artifacts,
   signer/encryption keys, and AX Redis/ownership state separately; reconcile
   revoked grants, sessions, leases, active attempts, and any external writes.
   Measure RPO and RTO **to usable authorized service**, including these
   operator steps. A successful PostgreSQL PITR by itself does not meet the
   Blaxsmith enterprise recovery target.

Failure drills also need primary pod deletion, node loss, and the declared zone
loss with client reconnection and acknowledged-write checks. Publish the
observed durability/availability policy, scope, backup/restore times, and
unsupported failure domains before calling this profile HA.
