ARG RUNTIME_PROOF_IMAGE
FROM ${RUNTIME_PROOF_IMAGE}

USER root
# The dated Debian archive fixes the Git dependency set at build time. The
# final image contains no package manager invocation during task execution.
RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20260901T000000Z/ bookworm main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/20260901T000000Z/ bookworm-security main' \
      > /etc/apt/sources.list \
    && rm -f /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=binaries --chmod=0755 /ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=binaries --chmod=0755 /blaxsmith-tool-worker /usr/local/bin/blaxsmith-tool-worker
COPY --from=binaries --chmod=0755 /blaxsmith-git-askpass /usr/local/bin/blaxsmith-git-askpass
COPY deploy/tool-worker/manifest.mjs /opt/blaxsmith/manifest.mjs
RUN mkdir -p /run/blaxsmith /workspace \
    && chmod 0700 /run/blaxsmith \
    && node /opt/blaxsmith/verify.mjs \
    && node /opt/blaxsmith/manifest.mjs >/dev/null

WORKDIR /workspace
CMD ["/usr/local/bin/ax-task-runner"]
