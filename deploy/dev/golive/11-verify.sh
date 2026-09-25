#!/usr/bin/env bash
# NODE. Post-apply checks: migrations, env, Substrate token, allowlist, router, pool, policy.
source "$(dirname "$0")/env.sh"
n=blaxsmith-preview
kubectl -n $n get pods -l app.kubernetes.io/name=blaxsmith-app -o wide
kubectl -n $n logs deploy/preview-app -c app --tail=40 | grep -v 'TLS handshake error' || true
kubectl -n $n exec preview-postgres-0 -- sh -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "select version from blaxsmith_schema_migrations order by 1 desc limit 5"'
kubectl -n $n get deploy preview-app -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep -E '^BLAXSMITH_(GUEST_ROUTER|DISPATCH_WORKER_IMAGE)='
test "$(kubectl -n $n get deploy preview-app -o jsonpath='{.spec.template.spec.containers[0].image}')" = "$(cat "$STATE/app.image")"
# Re-minted Substrate token works from the app pod (output discarded).
kubectl -n $n exec deploy/preview-app -c app -- env KUBECTL_ATE_CA_FILE=/run/blaxsmith/dispatch/substrate-ca.pem \
  /opt/blaxsmith/dispatch-tools/kubectl-ate --endpoint api.ate-system.svc:443 \
  --token-file /run/blaxsmith/dispatch/substrate-token get atespaces >/dev/null && echo 'substrate token ok'
kubectl -n ax-system get deploy ax-controller -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep runner-image
kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl -n ax-system get workerpool blaxsmith-smoke -o jsonpath='{.spec.replicas} {.spec.template.resources}{"\n"}'
kubectl -n ate-system get networkpolicy atenet-router-callers
echo 'Now: UI model keys, private-Git connection + source, start Guild run; then:'
echo "  kubectl -n ax-system logs deploy/ax-controller --since=10m | grep -E 'RunnerImageDenied|WorkspaceSetup' || echo none"
