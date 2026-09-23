#!/bin/sh
set -eu

chart=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
api=example.invalid/blaxsmith-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
web=example.invalid/blaxsmith-web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
rendered=$(mktemp)
trap 'rm -f "$rendered"' EXIT

helm lint "$chart" --set-string apiImage="$api" --set-string webImage="$web"
helm template preview "$chart" --set-string apiImage="$api" --set-string webImage="$web" > "$rendered"
grep -F -q 'type: ClusterIP' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'runAsNonRoot: true' "$rendered"
grep -F -q 'readOnlyRootFilesystem: true' "$rendered"
grep -F -q 'nc -z 127.0.0.1 8001' "$rendered"
if helm template preview "$chart" --set-string apiImage=example.invalid/blaxsmith-api:latest --set-string webImage="$web" >/dev/null 2>&1; then
  echo 'mutable API image unexpectedly accepted' >&2
  exit 1
fi
echo 'preview chart checks passed'
