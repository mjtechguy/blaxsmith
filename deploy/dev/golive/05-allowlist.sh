#!/usr/bin/env bash
# NODE. B4: allow only the new tool-worker on the gated AX controller; keep key and other args.
source "$(dirname "$0")/env.sh"
WORKER=${WORKER:-$(cat "$STATE/worker.image")}
[[ $WORKER =~ ^127\.0\.0\.1:5001/blaxsmith-tool-worker@sha256:[0-9a-f]{64}$ ]]
kubectl -n ax-system get deploy ax-controller -o json > "$STATE/ax-controller-before.json"
python3 - "$WORKER" "$STATE/ax-controller-before.json" > "$STATE/controller-runner-patch.json" <<'PY'
import json, sys
img, d = sys.argv[1], json.load(open(sys.argv[2]))
c = next(c for c in d["spec"]["template"]["spec"]["containers"] if c["name"] == "controller")
assert any(a.startswith("--blaxsmith-bootstrap-public-key=") for a in c["args"])
args = [a for a in c["args"] if not a.startswith("--blaxsmith-runner-image=")] + ["--blaxsmith-runner-image=" + img]
print(json.dumps({"spec": {"template": {"spec": {"containers": [{"name": "controller", "args": args}]}}}}))
PY
kubectl -n ax-system patch deployment ax-controller --type=strategic --patch-file="$STATE/controller-runner-patch.json"
kubectl -n ax-system rollout status deployment/ax-controller --timeout=120s
kubectl -n ax-system get deploy ax-controller -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ',' '\n' | grep -F "$WORKER"
