#!/usr/bin/env bash
# NODE. Build/publish atenet with guest-router-auth.patch and turn on --guest-client-auth:
# guest gRPC (/ateenv.*: exec, files) then needs HTTPS + a TokenReview for the preview
# app's own ServiceAccount (kubelet-rotated projected token, --guest-client-username) on
# every router port; the bootstrap connector token is refused there. Run BEFORE the app
# switches to the HTTPS guest router (10), so the router already strips the bearer when
# the app starts sending it, and rerun after 10 for the positive probe. Idempotent:
# reuses the build, prints the Deployment diff, asks only when there is a change.
source "$(dirname "$0")/env.sh"
BUILD=${GUEST_ROUTER_BUILD:-$DEV/substrate-guest-router-auth}
APP_NS=blaxsmith-preview APP_SA=preview-app   # chart: <release>-app, dispatch.guestRouterToken.projected
export GUEST_CLIENT_USERNAME=${GUEST_CLIENT_USERNAME:-system:serviceaccount:$APP_NS:$APP_SA}
cd "$TREE"
want=$(sha256sum integrations/substrate/guest-router-auth.patch | cut -d' ' -f1)
test -e "$BUILD/provenance.json" || bash integrations/substrate/build.sh "$DEV/substrate" "$BUILD"
python3 - "$BUILD" "$want" <<'PY'
import json, pathlib, sys
p = json.loads((pathlib.Path(sys.argv[1]) / 'provenance.json').read_text())
for k in ('router_auth_patch_sha256', 'actor_fence_patch_sha256', 'command_exit_router_auth_patch_sha256'):
    assert p.get(k), 'router build lacks ' + k
assert p.get('guest_router_auth_patch_sha256') == sys.argv[2], 'stale build: remove ' + sys.argv[1] + ' and rerun'
PY
test -e "$BUILD/atenet.image" || bash deploy/dev/publish-atenet.sh "$BUILD"
change=$(ROUTER_AUTH_DIFF=1 python3 deploy/dev/enable-bootstrap-router-auth.py "$BUILD")
if [ -n "$change" ]; then
  echo "$change"
  kubectl -n ate-system get deploy atenet-router -o json > "$STATE/atenet-router-before-guest-auth-$(date +%s).json"
  read -r -p "patch atenet-router? [y/N] " ok; [ "$ok" = y ]
  python3 deploy/dev/enable-bootstrap-router-auth.py "$BUILD"
else
  echo "atenet-router already configured"
fi
test "$(kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="atenet-router")].image}')" \
  = "$(cat "$BUILD/atenet.image")"
args=$(kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[?(@.name=="atenet-router")].args}')
grep -q -- '--guest-client-auth' <<<"$args"
grep -q -- "--guest-client-username=$GUEST_CLIENT_USERNAME" <<<"$args"
# Refused before actor resume. The method does not exist, so even a regression reaches
# only Unimplemented in the guest daemon. Tokens go through owner-only header files, never argv.
grpc=/ateenv.v1alpha.ProcessService/BlaxsmithAuthProbe
hdr=(-H "ate-target-actor: $GATE_SPACE/$GATE_TASK" -H 'content-type: application/grpc' -H 'te: trailers')
tls=(--http2 --cacert "$DEV/dispatch-router-ca.pem" --resolve "atenet-router.ate-system.svc:443:$ROUTER_IP")
url=https://atenet-router.ate-system.svc$grpc
umask 077; t=$(mktemp -d); trap 'rm -rf "$t"' EXIT
# Envoy answers gRPC denials as HTTP 200 with the status in grpc-status.
gs() { curl -s -o /dev/null -D - "$@" | tr -d '\r' | sed -n 's/^grpc-status: //Ip' | head -1; }
code=$(gs --http2-prior-knowledge -X POST "${hdr[@]}" "http://$ROUTER_IP$grpc")
echo "plaintext guest gRPC -> grpc-status $code"; test "$code" = 2          # requires TLS
code=$(gs -X POST "${hdr[@]}" "${tls[@]}" "$url")
echo "HTTPS guest gRPC without token -> grpc-status $code"; test "$code" = 16   # unauthenticated
printf 'Authorization: Bearer %s\n' "$(kubectl -n "$APP_NS" get secret preview-app-dispatch \
  -o jsonpath='{.data.bootstrap-token}' | base64 -d)" > "$t/connector"
code=$(gs -X POST "${hdr[@]}" -H @"$t/connector" "${tls[@]}" "$url")
echo "HTTPS guest gRPC with the bootstrap connector token -> grpc-status $code"; test "$code" = 7  # denied
if kubectl -n "$APP_NS" get sa "$APP_SA" >/dev/null 2>&1; then
  # Same identity and audience as the app's projected token; 10-minute TokenRequest.
  printf 'Authorization: Bearer %s\n' "$(kubectl -n "$APP_NS" create token "$APP_SA" \
    --audience=blaxsmith-bootstrap --duration=10m)" > "$t/app"
  echo "HTTPS guest gRPC as $GUEST_CLIENT_USERNAME (expect no grpc-status auth error; the gate runner answers 404):"
  curl -s -o /dev/null -D - -X POST "${hdr[@]}" -H @"$t/app" "${tls[@]}" "$url" | grep -i -E '^(HTTP|grpc-status)'
else
  echo "ServiceAccount $APP_NS/$APP_SA not yet created (step 10); rerun this script afterwards"
fi
rm -rf "$t"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "ate-target-actor: $GATE_SPACE/$GATE_TASK" "http://$ROUTER_IP/blaxsmith/command-exit")
echo "plaintext command-exit -> $code"; test "$code" = 426
kubectl -n ate-system logs deploy/atenet-router -c atenet-router --since=2m | grep -i -E 'error|denied' | tail -5 || true
# Next: S=$S bash 10-local-values-apply.sh (workstation), rerun this script, then 08-netpol.sh.
# Rollback: first clear dispatch.guestRouter on the app (terminals 503), or the app keeps
# sending its token to a router that would forward it into guests; then kubectl -n ate-system
# patch deploy atenet-router with the image/args from $STATE/atenet-router-before-guest-auth-*.json
# (enable-bootstrap-router-auth.py refuses to drop the flag).
