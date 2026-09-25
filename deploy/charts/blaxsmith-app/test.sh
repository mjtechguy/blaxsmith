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
grep -F -q 'type: RollingUpdate' "$rendered"
grep -F -q 'maxSurge: 1' "$rendered"
grep -F -q 'maxUnavailable: 0' "$rendered"
grep -F -q 'command: ["sleep", "5"]' "$rendered"
grep -F -q 'terminationGracePeriodSeconds: 30' "$rendered"
grep -F -A1 -- '- --session-idle-timeout' "$rendered" | grep -F -q '"168h"'
grep -F -A1 -- '- --session-absolute-lifetime' "$rendered" | grep -F -q '"720h"'
grep -F -A1 -- '- --shutdown-timeout' "$rendered" | grep -F -q '"20s"'
grep -F -A3 'readinessProbe:' "$rendered" | grep -F -q 'path: /healthz'
grep -F -A3 'livenessProbe:' "$rendered" | grep -F -q 'path: /livez'
helm template app "$chart" "$@" --set updateStrategy=Recreate --set shutdown.preStopSleepSeconds=0 \
  --set shutdown.drainTimeoutSeconds=60 --set-string session.idleTimeout=8h --set-string session.absoluteLifetime=24h > "$rendered"
grep -F -q 'type: Recreate' "$rendered"
if grep -F -q 'maxSurge' "$rendered" || grep -F -q 'preStop:' "$rendered"; then
  echo 'Recreate or a zero preStop sleep still rendered rolling or preStop settings' >&2
  exit 1
fi
grep -F -q 'terminationGracePeriodSeconds: 65' "$rendered"
grep -F -A1 -- '- --session-idle-timeout' "$rendered" | grep -F -q '"8h"'
grep -F -A1 -- '- --shutdown-timeout' "$rendered" | grep -F -q '"60s"'
for invalid in '--set updateStrategy=BlueGreen' '--set-string session.idleTimeout=7d' \
  '--set-string session.absoluteLifetime=' '--set shutdown.drainTimeoutSeconds=0'; do
  # Intentional splitting: fixed test arguments.
  if helm template app "$chart" "$@" $invalid >/dev/null 2>&1; then
    echo "invalid session or shutdown values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
helm template app "$chart" "$@" > "$rendered"
if grep -F -q -- '--migrations' "$rendered" || grep -F -q 'kind: Job' "$rendered"; then
  echo 'single-node defaults unexpectedly enable explicit migrations' >&2
  exit 1
fi
if grep -E -q 'kind: PodDisruptionBudget|topologySpreadConstraints:|minReadySeconds:' "$rendered"; then
  echo 'single-node defaults unexpectedly enable HA placement' >&2
  exit 1
fi
grep -F -q 'type: ClusterIP' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'readOnlyRootFilesystem: true' "$rendered"
grep -F -q 'scheme: HTTPS' "$rendered"
if grep -F -q 'BLAXSMITH_RUN_LAUNCH_ENABLED' "$rendered"; then
  echo 'application chart unexpectedly exposes an unbacked run-launch switch' >&2
  exit 1
fi
if grep -F -q 'name: ax-tunnel' "$rendered" || grep -F -q 'BLAXSMITH_DISPATCH_AX_SERVER' "$rendered"; then
  echo 'dispatch or the AX tunnel is unexpectedly enabled by default' >&2
  exit 1
fi
grep -F -q 'name: BLAXSMITH_DATABASE_URL' "$rendered"
grep -F -q 'defaultMode: 288' "$rendered"
if grep -F -q 'name: database-ca' "$rendered"; then
  echo 'database CA unexpectedly mounted by default' >&2
  exit 1
fi
helm template app "$chart" "$@" --set-string databaseCASecretName=pg-ca > "$rendered"
grep -F -q 'name: database-ca' "$rendered"
grep -F -q 'secretName: "pg-ca"' "$rendered"
grep -F -q 'mountPath: /run/blaxsmith/database-ca' "$rendered"
grep -F -q 'key: ca.crt' "$rendered"
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
grep -F -q 'type: RollingUpdate' "$rendered"
grep -F -q -- '--migrations' "$rendered"
grep -F -q -- '- verify' "$rendered"
grep -F -q 'minDomains: 2' "$rendered"
grep -F -q 'topologyKey: kubernetes.io/hostname' "$rendered"
grep -F -q 'whenUnsatisfiable: DoNotSchedule' "$rendered"
helm template app "$chart" "$@" --set ha.enabled=true --set replicaCount=2 \
  --kube-version 1.30.0 > "$rendered"
grep -F -q 'replicas: 2' "$rendered"
grep -F -q 'kind: PodDisruptionBudget' "$rendered"
helm template app "$chart" "$@" --set ha.enabled=true --set replicaCount=3 \
  --set-string databaseCASecretName=pg-ca --set-string migrationJob.name=app-migrate-123 \
  --kube-version 1.30.0 --show-only templates/migration-job.yaml > "$rendered"
grep -F -q 'kind: Job' "$rendered"
grep -F -q 'name: "app-migrate-123"' "$rendered"
grep -F -q 'args: [migrate, --require-verified-database]' "$rendered"
grep -F -q 'secretName: "pg-ca"' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'backoffLimit: 0' "$rendered"
if helm template app "$chart" "$@" --set-string migrationJob.name='BAD_NAME' >/dev/null 2>&1; then
  echo 'invalid migration job name accepted' >&2
  exit 1
fi
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
dispatch_args='--set dispatch.enabled=true
--set-string accessKeySecretName=app-access-key
--set-string dispatch.credentialsSecretName=app-dispatch
--set-string dispatch.tools.image=example.invalid/dispatch-tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
--set-string dispatch.ax.cliSHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
--set-string dispatch.substrate.cliSHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
--set-string dispatch.substrate.endpoint=api.ate-system.svc:443
--set-string dispatch.bootstrap.routerURL=https://router.ate-system.svc:443
--set-string dispatch.clusterID=dev-cluster
--set-string dispatch.leaseTTL=10m
--set-string dispatch.workerImage=example.invalid/blaxsmith-worker@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
--set-string dispatch.workerPool=blaxsmith-pool
--set-string dispatch.snapshotStorage=blaxsmith-snapshots
--set-string dispatch.workspace=blaxsmith-workspaces
--set-string dispatch.gateway=blaxsmith-egress'
# Intentional splitting: these are fixed chart fixture arguments, not user input.
helm template app "$chart" "$@" $dispatch_args > "$rendered"
grep -F -q -- '- --enable-dispatch' "$rendered"
grep -F -q 'name: copy-dispatch-tools' "$rendered"
grep -F -q 'mountPath: /dispatch-tools' "$rendered"
grep -F -q 'mountPath: /opt/blaxsmith/dispatch-tools' "$rendered"
grep -F -q 'readOnly: true' "$rendered"
grep -F -q 'value: /opt/blaxsmith/dispatch-tools/ax' "$rendered"
grep -F -q 'value: /opt/blaxsmith/dispatch-tools/kubectl-ate' "$rendered"
grep -F -q 'name: BLAXSMITH_DISPATCH_AX_SERVER' "$rendered"
grep -F -q 'value: "http://127.0.0.1:18443"' "$rendered"
grep -F -q 'name: ax-tunnel' "$rendered"
grep -F -q 'TCP-LISTEN:18443,bind=127.0.0.1,reuseaddr,fork' "$rendered"
grep -F -q 'TCP:ax-server.ax-system.svc.cluster.local:8080' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -q 'name: BLAXSMITH_DISPATCH_SUBSTRATE_TOKEN_FILE' "$rendered"
grep -F -q 'path: bootstrap-signer' "$rendered"
grep -F -q 'defaultMode: 288' "$rendered"
if grep -F -q -- '--allow-open-egress-dev' "$rendered"; then
  echo 'open AX egress was enabled without the explicit dev switch' >&2
  exit 1
fi
if grep -F -q 'BLAXSMITH_GUEST_ROUTER' "$rendered"; then
  echo 'guest router unexpectedly enabled by default' >&2
  exit 1
fi
helm template app "$chart" "$@" $dispatch_args \
  --set-string dispatch.guestRouter=router.ate-system.svc:443 > "$rendered"
grep -F -q 'value: "router.ate-system.svc:443"' "$rendered"
grep -F -A1 'name: BLAXSMITH_GUEST_ROUTER_CA_FILE' "$rendered" | grep -F -q '/run/blaxsmith/dispatch/router-ca.pem'
grep -F -A1 'name: BLAXSMITH_GUEST_ROUTER_TOKEN_FILE' "$rendered" | grep -F -q '/run/blaxsmith/dispatch/bootstrap-token'
if grep -F -q 'kind: ServiceAccount' "$rendered" || grep -F -q 'serviceAccountName:' "$rendered"; then
  echo 'guest router ServiceAccount rendered without the projected token option' >&2
  exit 1
fi
if helm template app "$chart" "$@" --set-string dispatch.guestRouter=router.ate-system.svc:443 >/dev/null 2>&1; then
  echo 'guest router unexpectedly accepted without dispatch credentials' >&2
  exit 1
fi
helm template app "$chart" "$@" $dispatch_args --set-string dispatch.guestRouter=router.ate-system.svc:443 \
  --set dispatch.guestRouterToken.projected=true > "$rendered"
grep -F -q 'kind: ServiceAccount' "$rendered"
grep -F -q 'serviceAccountName: app-app' "$rendered"
grep -F -q 'automountServiceAccountToken: false' "$rendered"
grep -F -A1 'name: BLAXSMITH_GUEST_ROUTER_TOKEN_FILE' "$rendered" | grep -F -q '/run/blaxsmith/guest-router/token'
grep -F -q 'mountPath: /run/blaxsmith/guest-router' "$rendered"
grep -F -A3 'serviceAccountToken:' "$rendered" | grep -F -q 'audience: "blaxsmith-bootstrap"'
grep -F -A3 'serviceAccountToken:' "$rendered" | grep -F -q 'expirationSeconds: 3600'
for invalid in '--set dispatch.guestRouterToken.expirationSeconds=300' \
  '--set-string dispatch.guestRouterToken.audience=' '--set-string dispatch.guestRouter='; do
  if helm template app "$chart" "$@" $dispatch_args --set-string dispatch.guestRouter=router.ate-system.svc:443 \
    --set dispatch.guestRouterToken.projected=true $invalid >/dev/null 2>&1; then
    echo "invalid projected guest token values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
helm template app "$chart" "$@" $dispatch_args --set dispatch.egressMode=open-dev \
  --set dispatch.dev.allowOpenEgress=true > "$rendered"
grep -F -q -- '- --enable-dispatch' "$rendered"
grep -F -q -- '- --allow-open-egress-dev' "$rendered"
grep -F -q 'name: BLAXSMITH_DISPATCH_EGRESS_MODE' "$rendered"
grep -F -q 'value: "open-dev"' "$rendered"
if helm template app "$chart" "$@" $dispatch_args --set dispatch.egressMode=open-dev >/dev/null 2>&1; then
  echo 'open AX egress unexpectedly accepted without the dev switch' >&2
  exit 1
fi
if helm template app "$chart" "$@" $dispatch_args --set dispatch.dev.allowOpenEgress=true >/dev/null 2>&1; then
  echo 'dev open-egress switch unexpectedly accepted with exact mode' >&2
  exit 1
fi
if helm template app "$chart" "$@" $dispatch_args \
  --set-string dispatch.tools.image=example.invalid/dispatch-tools:latest >/dev/null 2>&1; then
  echo 'mutable dispatch tools image unexpectedly accepted' >&2
  exit 1
fi
if helm template app "$chart" "$@" $dispatch_args \
  --set-string accessKeySecretName= >/dev/null 2>&1; then
  echo 'dispatch unexpectedly accepted without the credential-encryption key' >&2
  exit 1
fi
helm template app "$chart" "$@" > "$rendered"
if grep -F -q 'blaxsmith-gw' "$rendered" || grep -F -q 'BLAXSMITH_GATEWAY_URL' "$rendered"; then
  echo 'model gateway is unexpectedly enabled by default' >&2
  exit 1
fi
gateway_args='--set gateway.enabled=true
--set-string gateway.publicURL=https://gw.example.test
--set-string gateway.tlsSecretName=gw-tls
--set-string accessKeySecretName=app-access-key'
# Intentional splitting: fixed chart fixture arguments.
helm lint "$chart" "$@" $gateway_args
helm template app "$chart" "$@" $gateway_args > "$rendered"
grep -F -q 'name: app-gw' "$rendered"
grep -F -q -- '- gateway' "$rendered"
grep -F -q -- '- /run/blaxsmith/gateway-tls/tls.crt' "$rendered"
grep -F -q 'kind: NetworkPolicy' "$rendered"
grep -F -q 'policyTypes: ["Ingress", "Egress"]' "$rendered"
grep -F -q 'name: BLAXSMITH_GATEWAY_URL' "$rendered"
grep -F -q 'value: "https://gw.example.test"' "$rendered"
grep -F -q 'terminationGracePeriodSeconds: 915' "$rendered"
if grep -F -q -- '--allow-plaintext' "$rendered"; then
  echo 'gateway with a TLS secret unexpectedly serves plaintext' >&2
  exit 1
fi
helm template app "$chart" "$@" $gateway_args --set-string gateway.tlsSecretName= \
  --set gateway.networkPolicy.enabled=false > "$rendered"
grep -F -q -- '- --allow-plaintext' "$rendered"
if grep -F -q 'kind: NetworkPolicy' "$rendered"; then
  echo 'gateway NetworkPolicy rendered while disabled' >&2
  exit 1
fi
for invalid in '--set-string gateway.publicURL=' '--set-string gateway.publicURL=https://gw.example.test/path' \
  '--set-string accessKeySecretName='; do
  if helm template app "$chart" "$@" $gateway_args $invalid >/dev/null 2>&1; then
    echo "invalid gateway values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
helm template app "$chart" "$@" --set-string accessKeySecretName=app-access-key > "$rendered"
if grep -F -q 'BLAXSMITH_ACCESS_KEY_ID' "$rendered" || grep -F -q 'previous-access-key' "$rendered"; then
  echo 'access key rotation settings rendered while unset' >&2
  exit 1
fi
rotation_args='--set-string accessKeySecretName=app-access-key
--set-string accessKeyID=key-2
--set previousAccessKeys[0].id=primary
--set-string previousAccessKeys[0].secretName=old-access-key'
# Intentional splitting: fixed chart fixture arguments.
helm lint "$chart" "$@" $rotation_args $gateway_args
helm template app "$chart" "$@" $rotation_args $gateway_args > "$rendered"
[ "$(grep -F -c 'value: "key-2"' "$rendered")" -eq 2 ]
[ "$(grep -F -c 'value: "primary=/run/blaxsmith/previous-access-keys/primary/key"' "$rendered")" -eq 2 ]
[ "$(grep -F -c 'mountPath: /run/blaxsmith/previous-access-keys/primary' "$rendered")" -eq 2 ]
[ "$(grep -F -c 'secretName: "old-access-key"' "$rendered")" -eq 2 ]
for invalid in '--set-string accessKeySecretName=' '--set previousAccessKeys[1].id=primary --set previousAccessKeys[1].secretName=x' \
  '--set previousAccessKeys[1].id=key-2 --set previousAccessKeys[1].secretName=x' '--set previousAccessKeys[0].id=bad/id' \
  '--set-string previousAccessKeys[0].secretName=' '--set-string accessKeyID=bad/id'; do
  if helm template app "$chart" "$@" $rotation_args $invalid >/dev/null 2>&1; then
    echo "invalid access key rotation values unexpectedly accepted: $invalid" >&2
    exit 1
  fi
done
echo 'application chart checks passed'
