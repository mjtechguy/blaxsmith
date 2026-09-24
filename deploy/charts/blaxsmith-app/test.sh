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
grep -F -q 'type: Recreate' "$rendered"
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
grep -F -q 'type: Recreate' "$rendered"
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
echo 'application chart checks passed'
