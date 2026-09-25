#!/usr/bin/env bash
# NODE. B5: publish atenet with command-exit-router-auth, enable router auth, re-probe.
source "$(dirname "$0")/env.sh"
python3 - "$ROUTER_BUILD" <<'PY'
import json, pathlib, sys
p = json.loads((pathlib.Path(sys.argv[1]) / 'provenance.json').read_text())
for k in ('router_auth_patch_sha256', 'actor_fence_patch_sha256', 'command_exit_router_auth_patch_sha256'):
    assert p.get(k), 'router build lacks ' + k
PY
kubectl -n ate-system get deploy atenet-router -o json > "$STATE/atenet-router-before.json"
cd "$TREE"
test -e "$ROUTER_BUILD/atenet.image" || bash deploy/dev/publish-atenet.sh "$ROUTER_BUILD"
python3 deploy/dev/enable-bootstrap-router-auth.py "$ROUTER_BUILD"
test "$(kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="atenet-router")].image}')" \
  = "$(cat "$ROUTER_BUILD/atenet.image")"
# Plaintext command-exit must now be refused before routing (expect 426, not 2xx/404).
code=$(curl -s -o /dev/null -w '%{http_code}' -H "ate-target-actor: $GATE_SPACE/$GATE_TASK" "http://$ROUTER_IP/blaxsmith/command-exit")
echo "plaintext command-exit -> $code"; test "$code" = 426
python3 deploy/dev/probe-attestation.py "$GATE_SPACE" "$GATE_TASK" "$ROUTER_IP" \
  "$DEV/router-auth-probe-evidence-$S-$(date +%s)"
# Rollback: kubectl -n ate-system patch deploy atenet-router with the image/args from $STATE/atenet-router-before.json
