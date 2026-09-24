# The guild runner variant: the pinned tool-worker image plus the runtime
# layers the Guild extension declares (uv, Python 3.12, Serena). Pins live in
# guild-runtimes.json; check-variant.py keeps them, this file, and the Guild
# manifest in step. Nothing here runs when an AX task starts.
ARG TOOL_WORKER_IMAGE
FROM ${TOOL_WORKER_IMAGE}

USER root
ADD --checksum=sha256:89eadd7c76fc063887959510d5ba0ab1264dfd5f1143b925ddb73021a40acf16 \
    https://github.com/astral-sh/uv/releases/download/0.12.18/uv-x86_64-unknown-linux-gnu.tar.gz /tmp/uv.tar.gz
ENV UV_PYTHON_INSTALL_DIR=/opt/blaxsmith/python \
    UV_TOOL_DIR=/opt/blaxsmith/uv-tools \
    UV_TOOL_BIN_DIR=/usr/local/bin
# uv's own Python download list is fixed by the pinned uv binary, and
# --exclude-newer fixes Serena's transitive dependency set to one date. The
# build cache is discarded so tasks start with their own empty cache.
RUN export UV_CACHE_DIR=/tmp/uv-build-cache \
    && tar -xzf /tmp/uv.tar.gz -C /tmp \
    && install -m 0755 /tmp/uv-x86_64-unknown-linux-gnu/uv /tmp/uv-x86_64-unknown-linux-gnu/uvx /usr/local/bin/ \
    && rm -rf /tmp/uv.tar.gz /tmp/uv-x86_64-unknown-linux-gnu \
    && uv python install 3.12 \
    && uv tool install --python 3.12 --exclude-newer 2026-09-01T00:00:00Z serena-agent==1.7.0 \
    && rm -rf /tmp/uv-build-cache \
    && chmod -R a+rX /opt/blaxsmith/python /opt/blaxsmith/uv-tools
COPY deploy/tool-worker/guild-runtimes.json /opt/blaxsmith/runtimes.json
COPY deploy/tool-worker/runtimes.mjs /opt/blaxsmith/runtimes.mjs
# At task time uv may use only the preinstalled interpreter; package
# resolution (foundry-mcp) still uses the egress the extension declares.
ENV UV_PYTHON_DOWNLOADS=never
RUN --network=none UV_CACHE_DIR=/tmp/uv-check-cache node /opt/blaxsmith/runtimes.mjs >/dev/null \
    && rm -rf /tmp/uv-check-cache
