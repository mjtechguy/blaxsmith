#!/bin/sh
set -eu

chart=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
image=example.invalid/postgresql@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
set -- --set-string postgresImage="$image" --set-string storageClass=durable-csi \
  --set-string storageSize=20Gi --set-string appSecretName=pg-app \
  --set-string serverTLSSecretName=pg-tls --set-string serverCASecretName=pg-ca \
  --set-string backup.destinationPath=s3://backup.example/blaxsmith/production \
  --set-string backup.credentialsSecretName=pg-backup \
  --set-string backup.retentionPolicy=30d \
  --set-string backup.schedule='0 0 2 * * *'

if helm template pg "$chart" >/dev/null 2>&1; then
  echo 'CNPG chart unexpectedly accepted missing production values' >&2
  exit 1
fi
helm lint "$chart" "$@"
rendered=$(mktemp)
trap 'rm "$rendered"' EXIT
helm template pg "$chart" "$@" --namespace blaxsmith > "$rendered"
grep -F -q 'kind: ObjectStore' "$rendered"
grep -F -q 'kind: Cluster' "$rendered"
grep -F -q 'kind: ScheduledBackup' "$rendered"
grep -F -q 'isWALArchiver: true' "$rendered"
grep -F -q 'barmanObjectName: pg-archive' "$rendered"
grep -F -q 'podAntiAffinityType: required' "$rendered"
grep -F -q 'serverCASecret: "pg-ca"' "$rendered"
grep -F -q '    tls:' "$rendered"
grep -F -q '      enabled: true' "$rendered"
grep -F -q 'method: plugin' "$rendered"
grep -F -q 'immediate: true' "$rendered"
if grep -F -q 'kind: PodMonitor' "$rendered"; then
  echo 'PodMonitor unexpectedly enabled without Prometheus Operator' >&2
  exit 1
fi
helm template pg "$chart" "$@" --set monitoring.podMonitor=true \
  --set-string backup.endpointURL=https://store.example.test \
  --set-string backup.endpointCASecretName=store-ca > "$rendered"
grep -F -q 'kind: PodMonitor' "$rendered"
grep -F -q 'scheme: https' "$rendered"
grep -F -q 'name: "store-ca"' "$rendered"
for invalid in '--set-string backup.endpointURL=http://store.example.test' \
  '--set-string postgresImage=example.invalid/postgresql:latest' \
  '--set-string backup.destinationPath=s3://bucket/' \
  '--set-string backup.destinationPath=' \
  '--set-string backup.credentialsSecretName='; do
  # Intentional splitting of fixed test arguments.
  if helm template pg "$chart" "$@" $invalid >/dev/null 2>&1; then
    echo "unsafe CNPG values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
echo 'CNPG profile render checks passed'
