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
grep -F -q 'type: ClusterIP' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'readOnlyRootFilesystem: true' "$rendered"
grep -F -q 'scheme: HTTPS' "$rendered"
grep -F -q 'name: BLAXSMITH_DATABASE_URL' "$rendered"
grep -F -q 'defaultMode: 288' "$rendered"
if helm template app "$chart" --set-string image=example.invalid/blaxsmith-app:latest \
  --set-string origin=https://app.example.test --set-string tlsSecretName=app-tls \
  --set-string signerSecretName=app-signer --set-string databaseSecretName=app-database >/dev/null 2>&1; then
  echo 'mutable application image unexpectedly accepted' >&2
  exit 1
fi
echo 'application chart checks passed'
