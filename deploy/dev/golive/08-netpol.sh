#!/usr/bin/env bash
# NODE. Only preview-app + ax-controller may reach atenet-router data ports.
source "$(dirname "$0")/env.sh"
kubectl apply --dry-run=server -f "$(dirname "$0")/atenet-router-callers.yaml"
kubectl apply -f "$(dirname "$0")/atenet-router-callers.yaml"
sleep 5
probe='127.0.0.1:5001/blaxsmith-dispatch-tools@sha256:f5824b7b21e60327e9d336a7023d054e818f0c2c85fb9f78813cec1dad191079'
for port in 80 443; do
  r=$(kubectl -n default run "np-probe-$port" --rm -i --restart=Never --image="$probe" --command -- \
      sh -c "nc -z -w3 atenet-router.ate-system.svc.cluster.local $port && echo OPEN || echo BLOCKED" | head -1)
  echo "default-ns -> router:$port $r"; test "$r" = BLOCKED
done
kubectl -n ax-system exec deploy/ax-controller -- nc -z -w3 atenet-router.ate-system.svc.cluster.local 80
kubectl -n blaxsmith-preview exec deploy/preview-app -c app -- nc -z -w3 atenet-router.ate-system.svc.cluster.local 80
kubectl -n blaxsmith-preview exec deploy/preview-app -c app -- nc -z -w3 atenet-router.ate-system.svc.cluster.local 443
kubectl -n ate-system get pods -l app=atenet-router   # still Ready (kubelet probes unaffected)
kubectl -n ax-system logs deploy/ax-controller --since=2m | grep -i -E 'router|refused|i/o timeout' | tail -5 || true
# Rollback: kubectl -n ate-system delete networkpolicy atenet-router-callers
