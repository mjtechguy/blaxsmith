# Substrate egress compatibility patch

Upstream: `github.com/agent-substrate/substrate`, commit
`672533541dbfcd29084e4de2475267088bda3651`, Apache-2.0 (see LICENSE).
`egress-policy.patch` changes only the egress gateway handler and its tests. The
reference checkout stays untouched. Rebuild with:

```sh
bash integrations/substrate/build.sh ../reference/substrate /tmp/blaxsmith-substrate-build
```

The build exports the pinned commit, applies the patch, runs the `atenet` tests
and `go vet`, builds Linux/AMD64 `atenet`, and records source/patch/binary hashes.
On the prepared development node, `deploy/dev/publish-atenet.sh` adds that binary
to a pinned Alpine image. Deploy its digest to the `atenet-egress` `ext-proc`
container with `/usr/local/bin/atenet` as command. The ingress gateway is not
changed. [Linux build provenance](provenance.json) and the [live AX/gVisor
probe](../../docs/egress-probe.json) record this tested combination.

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
