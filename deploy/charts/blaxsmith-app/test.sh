#!/bin/sh
set -eu

chart=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
image=example.invalid/blaxsmith-app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
set -- --set-string image="$image" --set-string origin=https://app.example.test \
  --set-string tlsSecretName=app-tls --set-string signerSecretName=app-signer \
  --set-string databaseSecretName=app-database

helm lint "$chart" "$@"
rendered=$(mktemp)
trap 'rm "$rendered"' EXIT
helm template app "$chart" "$@" > "$rendered"
grep -F -q 'replicas: 1' "$rendered"
if grep -E -q 'kind: PodDisruptionBudget|topologySpreadConstraints:|minReadySeconds:' "$rendered"; then
  echo 'single-node defaults unexpectedly enable HA placement' >&2
  exit 1
fi
grep -F -q 'type: ClusterIP' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'readOnlyRootFilesystem: true' "$rendered"
grep -F -q 'scheme: HTTPS' "$rendered"
grep -F -q 'name: BLAXSMITH_DATABASE_URL' "$rendered"
grep -F -q 'defaultMode: 288' "$rendered"
helm template app "$chart" "$@" --set-string previousSignerPublicSecretName=prior-key > "$rendered"
grep -F -q -- '--previous-signer-public-file' "$rendered"
grep -F -q 'secretName: "prior-key"' "$rendered"
helm lint "$chart" "$@" --set ha.enabled=true --set replicaCount=3
helm template app "$chart" "$@" --set ha.enabled=true --set replicaCount=3 \
  --kube-version 1.30.0 > "$rendered"
grep -F -q 'kind: PodDisruptionBudget' "$rendered"
grep -F -q 'maxUnavailable: 1' "$rendered"
grep -F -q 'replicas: 3' "$rendered"
grep -F -q 'minReadySeconds: 5' "$rendered"
grep -F -q 'maxSurge: 1' "$rendered"
grep -F -q 'minDomains: 2' "$rendered"
grep -F -q 'topologyKey: kubernetes.io/hostname' "$rendered"
grep -F -q 'whenUnsatisfiable: DoNotSchedule' "$rendered"
helm template app "$chart" "$@" --set ha.enabled=true --set replicaCount=2 \
  --kube-version 1.30.0 > "$rendered"
grep -F -q 'replicas: 2' "$rendered"
grep -F -q 'kind: PodDisruptionBudget' "$rendered"
for invalid in '--set replicaCount=0' '--set replicaCount=1.5' \
  '--set replicaCount=2' '--set ha.enabled=true --set replicaCount=1' \
  '--set-string ha.enabled=true'; do
  # Intentional splitting: these are fixed test arguments, not user input.
  if helm template app "$chart" "$@" $invalid >/dev/null 2>&1; then
    echo "invalid chart values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
if helm template app "$chart" "$@" --set ha.enabled=true --set replicaCount=2 \
  --kube-version 1.29.9 >/dev/null 2>&1; then
  echo 'HA mode unexpectedly accepted Kubernetes before 1.30' >&2
  exit 1
fi
if helm template app "$chart" --set-string image=example.invalid/blaxsmith-app:latest \
  --set-string origin=https://app.example.test --set-string tlsSecretName=app-tls \
  --set-string signerSecretName=app-signer --set-string databaseSecretName=app-database >/dev/null 2>&1; then
  echo 'mutable application image unexpectedly accepted' >&2
  exit 1
fi
echo 'application chart checks passed'
