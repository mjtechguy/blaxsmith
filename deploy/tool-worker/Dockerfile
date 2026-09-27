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
    && apt-get install -y --no-install-recommends git ca-certificates tmux bsdutils util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && tmux -V | awk '{ split($2, v, "."); if (v[1] < 3 || (v[1] == 3 && v[2] + 0 < 2)) { print "tmux 3.2+ required (attach -f ignore-size), got " $0; exit 1 } }' \
    && script --version

COPY --from=binaries --chmod=0755 /ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=binaries --chmod=0755 /blaxsmith-tool-worker /usr/local/bin/blaxsmith-tool-worker
COPY --from=binaries --chmod=0755 /blaxsmith-git-askpass /usr/local/bin/blaxsmith-git-askpass
COPY --from=binaries /blaxsmith-git-ca.pem /tmp/blaxsmith-git-ca.pem
COPY deploy/tool-worker/manifest.mjs deploy/tool-worker/runtimes.mjs /opt/blaxsmith/
RUN mkdir -p /run/blaxsmith /workspace \
    && ln -s blaxsmith-tool-worker /usr/local/bin/bx \
    && chmod 0700 /run/blaxsmith \
    && cp /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/blaxsmith-git-ca.pem \
    && if [ -s /tmp/blaxsmith-git-ca.pem ]; then cat /tmp/blaxsmith-git-ca.pem >> /etc/ssl/certs/blaxsmith-git-ca.pem; fi \
    && rm /tmp/blaxsmith-git-ca.pem \
    && node /opt/blaxsmith/verify.mjs \
    && node /opt/blaxsmith/manifest.mjs >/dev/null

WORKDIR /workspace
CMD ["/usr/local/bin/ax-task-runner"]
