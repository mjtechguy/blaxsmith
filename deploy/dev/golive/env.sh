# Sourced by every node-side step. Usage on the node: S=<7-char sha> bash NN-step.sh
set -euo pipefail
: "${S:?set S to the 7-char commit being deployed}"
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
DEV=/opt/blaxsmith-dev
TREE=${TREE:-$DEV/blaxsmith-mvp-$S}                       # clean worktree for builds and scripts
STATE=$DEV/mvp-state-$S                          # worker.image, app.image, patches, backups
AX=$DEV/ax-build-f009-linux                      # verified AX build of the live controller
ROUTER_BUILD=$DEV/substrate-bootstrap-phase-f6320e6   # atenet with command-exit-router-auth
ROUTER_IP=10.43.36.216
GATE_SPACE=blaxsmith-gate-2bb31a39f4 GATE_TASK=gated-runner
mkdir -p "$STATE"; chmod 700 "$STATE"
