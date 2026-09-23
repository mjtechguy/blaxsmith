#!/bin/sh
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

chart=oci://registry-1.docker.io/bitnamicharts/redis
version=23.1.1
digest=sha256:f4a368f7a67f4f2bedee2426bfb063b960565ee38a91fdf07185a014c9e63406
helm pull "$chart" --version "$version" --untar --untardir "$tmp" >"$tmp/pull.log" 2>&1
grep -F -q "Digest: $digest" "$tmp/pull.log"
helm lint "$tmp/redis" -f "$here/values.candidate.yaml" > /dev/null
helm template ax-redis "$tmp/redis" --namespace ax-system \
  -f "$here/values.candidate.yaml" >"$tmp/rendered.yaml"

for expected in 'kind: StatefulSet' '  replicas: 3' 'kind: NetworkPolicy' \
  'kind: PodDisruptionBudget' '  maxUnavailable: 1' '  minDomains: 3' \
  '    appendonly yes' '    maxmemory-policy noeviction' \
  '    min-replicas-to-write 1' 'ARGS=("--port" "0")' \
  'ARGS+=("--tls-replication" "yes")' '  volumeClaimTemplates:' \
  '        storageClassName: durable-csi-replace' \
  '            secretName: ax-redis-auth-replace' \
  '            secretName: ax-redis-tls-replace' \
  'registry-1.docker.io/bitnami/redis@sha256:0000000000000000000000000000000000000000000000000000000000000000' \
  'registry-1.docker.io/bitnami/redis-sentinel@sha256:0000000000000000000000000000000000000000000000000000000000000000'; do
  grep -F -q "$expected" "$tmp/rendered.yaml"
done
if grep -Eq '^kind: Secret$|image: .*:latest$' "$tmp/rendered.yaml"; then
  echo 'Redis candidate rendered an unmanaged Secret or floating image' >&2
  exit 1
fi
test "$(grep -c 'storageClassName: durable-csi-replace' "$tmp/rendered.yaml")" -eq 2
echo 'AX Redis chart candidate render checks passed (no deployment qualification)'
