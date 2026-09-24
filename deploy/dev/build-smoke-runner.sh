#!/usr/bin/env bash
# Build a credential-free AMD64 AX runner image without Docker or model tools.
set -euo pipefail
ax_source=${1:?usage: build-smoke-runner.sh AX_SOURCE_DIRECTORY}
expected=f009cc81c9a571073bc1dd58cd2ed934bf2d5b1c
test "$(git -C "$ax_source" rev-parse HEAD)" = "$expected"
test -z "$(git -C "$ax_source" status --porcelain)"
runner_build=$(mktemp -d)
trap 'rm -rf "$runner_build"' EXIT
mkdir -p "$runner_build/usr/local/bin"
cd "$ax_source"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags='-s -w' -o "$runner_build/usr/local/bin/ax-task-runner" ./cmd/ax-task-runner
tar -C "$runner_build" -cf "$runner_build/runner.tar" usr/local/bin/ax-task-runner
go run github.com/google/go-containerregistry/cmd/crane@v0.21.7 append \
  --base alpine:3.24.2@sha256:d56c381f961d307a21b3ca004cf1e3910f106644aefb1f43e654c8a56c4fd395 \
  --new_layer "$runner_build/runner.tar" \
  --new_tag 127.0.0.1:5001/blaxsmith-smoke-runner:ax-f009cc8
