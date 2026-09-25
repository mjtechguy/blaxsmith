#!/usr/bin/env bash
# NODE. Fetch the bundle into blaxsmith-work and make a clean worktree (build.sh needs one).
source "$(dirname "$0")/env.sh"
REF=${BUNDLE_REF:-refs/heads/main}
cd "$DEV/blaxsmith-work"
git bundle verify "$DEV/blaxsmith-mvp-$S.bundle"
git fetch "$DEV/blaxsmith-mvp-$S.bundle" "$REF:refs/heads/mvp-$S"
test "$(git rev-parse --short=7 "mvp-$S")" = "$S"
test -e "$TREE" || git worktree add "$TREE" "mvp-$S"
test -z "$(git -C "$TREE" status --porcelain)" || { echo "worktree not clean" >&2; exit 2; }
git -C "$TREE" log --oneline -1
ls "$TREE/db/migrations" | tail -5
grep -q 'tmux' "$TREE/deploy/tool-worker/Dockerfile" && grep -q 'bx' "$TREE/deploy/tool-worker/Dockerfile"
grep -q 'BLAXSMITH_GUEST_ROUTER' "$TREE/deploy/charts/blaxsmith-app/templates/deployment.yaml" \
  || { echo "B2 missing: chart has no BLAXSMITH_GUEST_ROUTER" >&2; exit 2; }
