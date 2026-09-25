#!/usr/bin/env bash
# NODE. Pool = 3 x 2 CPU / 4 GiB (Task resources must match; integrator raises them).
source "$(dirname "$0")/env.sh"
kubectl -n ax-system get workerpool blaxsmith-smoke -o json > "$STATE/workerpool-before.json"
kubectl -n ax-system patch workerpool blaxsmith-smoke --type=merge -p \
  '{"spec":{"replicas":3,"template":{"resources":{"requests":{"cpu":"2","memory":"4Gi"},"limits":{"cpu":"2","memory":"4Gi"}}}}}'
kubectl -n ax-system rollout status deployment/blaxsmith-smoke --timeout=300s
kubectl -n ax-system get workerpool blaxsmith-smoke
kubectl -n ax-system get pods -l ate.dev/worker-pool=blaxsmith-smoke -o wide
kubectl describe node blaxsmith-dev1 | sed -n '/Allocated resources/,/Events/p'
