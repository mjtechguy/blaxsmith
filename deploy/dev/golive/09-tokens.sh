#!/usr/bin/env bash
# NODE. Re-mint the two connector tokens in preview-app-dispatch (minted 2026-09-24 13:10 UTC,
# TTL unknown; kubectl's default is 1h). Same identity/audiences as deploy/dev and
# docs/substrate-direct-ate-proof.json. Nothing is printed; temp files are owner-only and removed.
source "$(dirname "$0")/env.sh"
DUR=${TOKEN_DURATION:-24h}
umask 077
t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
kubectl -n ate-system create token blaxsmith-connector --audience=blaxsmith-bootstrap --duration="$DUR" > "$t/bootstrap-token"
kubectl -n ate-system create token blaxsmith-connector --audience=api.ate-system.svc --duration="$DUR" > "$t/substrate-token"
kubectl -n blaxsmith-preview get secret preview-app-dispatch -o json | python3 -c '
import base64, json, pathlib, sys
d = json.load(sys.stdin); t = pathlib.Path(sys.argv[1])
for k in ("bootstrap-token", "substrate-token"):
    v = (t / k).read_bytes().strip(); assert v.count(b".") == 2
    d["data"][k] = base64.b64encode(v).decode()
assert sorted(d["data"]) == ["actor-ca.pem", "bootstrap-signer", "bootstrap-token", "router-ca.pem", "substrate-ca.pem", "substrate-token"]
json.dump(d, sys.stdout)' "$t" | kubectl replace -f -
echo "re-minted for $DUR at $(date -u +%FT%TZ)" | tee "$STATE/tokens-minted.txt"
