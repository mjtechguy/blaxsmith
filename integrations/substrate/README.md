# Substrate egress and bootstrap patches

Upstream: `github.com/agent-substrate/substrate`, commit
`672533541dbfcd29084e4de2475267088bda3651`, Apache-2.0 (see LICENSE).
`egress-policy.patch` changes the egress gateway handler and tests.
`actor-attestation.patch` extends the gVisor worker's `atunnel` and its tests.
`bootstrap-router-auth.patch` restricts bootstrap ingress to the connector.
`bootstrap-actor-fence.patch` pins bootstrap routing to the actor UID.
`command-exit-router-auth.patch` applies the same connector authentication and
current-actor UID fence to the read-only command-exit route. The
`direct-ate-ca.patch` lets a connector use the pinned `kubectl-ate` over the
direct Substrate service with an explicit bearer-token file and mounted server
CA, without loading kubeconfig or querying the Kubernetes API. The build now
packages this patched CLI with the connector's runtime binaries.
The reference checkout stays untouched. Rebuild with:

```sh
bash integrations/substrate/build.sh ../reference/substrate /tmp/blaxsmith-substrate-build
```

The build exports the pinned commit, applies all six patches, runs focused tests
and `go vet`, builds Linux/AMD64 `atenet`, `ateom-gvisor`, and `kubectl-ate`,
and records source/patch/binary hashes.
On the prepared development node, `deploy/dev/publish-atenet.sh` adds that binary
to a pinned Alpine image. The first egress deployment used its digest in the
`atenet-egress` `ext-proc` container with `/usr/local/bin/atenet` as command.
[Linux build provenance](provenance.json) and the [live AX/gVisor
probe](../../docs/egress-probe.json) record this tested combination.

For actor attestation, `deploy/dev/publish-ateom.sh` adds the verified worker
binary to a digest-pinned upstream `ateom-gvisor` image. The
[attestation provenance](provenance-attestation.json) and
[live proof report](../../docs/actor-attestation-probe.json) record the tested
build. A fresh 32-byte connector nonce sent to the actor's challenge route
causes `atunnel` to sign that nonce and the exact guest response with its
activation-specific actor key. The response carries the actor certificate and
signature; the key never enters the gVisor guest. The product's
`internal/bootstrap.Verify` checks the cluster CA, actor UID/name/atespace,
certificate purpose, signature, nonce, and guest challenge lifetime. The live
probe rejected a wrong UID, changed nonce/response, wrong CA, malformed nonce,
and wrong actor route. It left the synthetic task suspended.

The router now requires verified HTTPS plus a Kubernetes TokenReview for any
`/blaxsmith/bootstrap/` route, with an exact service-account username and
`blaxsmith-bootstrap` audience. Missing, wrong-principal, wrong-audience, and
plaintext requests fail before actor resume. It removes the bearer token before
forwarding. Empty router configuration closes bootstrap routes. The
[live router probe](../../docs/bootstrap-router-auth-probe.json) and
[Linux provenance](provenance-router-auth.json) cover this follow-up. On the
dev node, `deploy/dev/bootstrap-router-auth.yaml` grants only the router
service account permission to create TokenReviews; the synthetic connector
service account has no Kubernetes API role. The test token is short-lived and
exists only in probe memory.

The [actor-fence overlay](bootstrap-actor-fence.patch) requires the authenticated
connector to name the expected actor UID. The router compares it to the actor
returned by resume and overwrites the forwarded header; `atunnel` compares it
again to its current activation before forwarding a bootstrap challenge or
release. Focused tests cover a missing or stale UID on both hops. The
[live proof](../../docs/bootstrap-actor-fence-proof.json) rejected a stale UID
before routing and still verified the current actor; the [gate probe](../../docs/bootstrap-actor-fence-gate.json)
rejected a stale UID and retained release/replay behavior across data-snapshot
resume. The [build provenance](provenance-actor-fence.json) records the pinned
Linux/AMD64 binary. This is a route fence, not a grant: the connector must get
the UID from the current scheduler assignment and keep that assignment fenced
through delivery. CONNECT tunnels and other ingress paths still need separate
security review before sensitive access.

The pinned CLI's direct-endpoint mode normally still loads kubeconfig to read
the live ClusterTrustBundle. `direct-ate-ca.patch` adds an explicit mounted-CA
path for a connector with a projected audience-scoped bearer token; it avoids
Kubernetes API credentials entirely. This narrows Kubernetes access only:
the [pinned authentication contract](https://github.com/agent-substrate/substrate/blob/672533541dbfcd29084e4de2475267088bda3651/docs/authentication.md)
says authenticated providers can call every RPC and authorization/RBAC are
not implemented. Treat the connector as a privileged trusted component for
its registered AX cluster, keep `ate-api` reachable only from that connector,
and do not claim per-atespace authorization until the pinned AX/Substrate API
enforces it.

The live direct-client probe in [the evidence report](../../docs/substrate-direct-ate-proof.json)
passed against the prepared Linux/AMD64 node with a deliberately missing
kubeconfig. It validates this CLI auth path only; connector deployment,
enrollment, and product workflow integration remain open.

The command-exit route overlay has focused router and atunnel tests for
connector-only access and stale actor UID rejection. It has not yet been
deployed or tested end-to-end on the node. The platform additionally checks
the released activation nonce and the AX/Substrate runtime before signing a
workflow receipt; the route itself does not make guest output trustworthy.

The standalone proof verifier is side-effect-free; the
[ledger](../../docs/bootstrap-ledger.md) now stores and consumes an
owner-bound nonce once in PostgreSQL tests. Authentication identifies the
connector, **not** an authorized attempt. The product connector must use that
ledger, recheck current actor/owner
and effective policy immediately before release, and prove encrypted private
Git setup. Until then, this endpoint carries no secret and only synthetic
tasks run on this node. Production connector enrollment and cross-cluster
transport remain unimplemented.

The gateway now checks the current actor egress policy on every new CONNECT,
after verifying the actor certificate, UID, and RUNNING state. No policy or no
matching rule denies access; control-plane failure also fails closed. It accepts
only an IP:port destination and implements `all` and canonical IPv4/IPv6 CIDR
rules. Unsupported hostname/effect rules and malformed policy data block the
whole policy. AX's companion patch rejects hostname and port restrictions
before storing them, because this IP-only CONNECT path cannot enforce those
claims and defaults missing gateways to deny-all. The network probe showed the
same reachable dev-registry endpoint allowed by `all` and its matching `/32`,
then denied by an empty policy, a nonmatching CIDR, and no gateway.

This checks **new CONNECTs**, not existing tunnels. It does not enforce ports,
hostnames, HTTP paths, DNS identity, or a complete network boundary around every
pod/path. It does not prove policy propagation latency, refresh/revocation of
credentials, or multi-tenant isolation. Keep this single-user cluster synthetic
until those controls and authenticated worker bootstrap are implemented and
tested. AX `GatewayReady` alone is not proof of dataplane enforcement.
