FROM node:24.21.0-bookworm-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6 AS deps
WORKDIR /opt/blaxsmith
COPY deploy/runtime-proof/package.json deploy/runtime-proof/package-lock.json ./
RUN test "$(uname -m)" = x86_64 \
    && test "$(node -p 'process.platform')" = linux \
    && test "$(node -p 'process.arch')" = x64 \
    && npm ci --omit=dev --include=optional --ignore-scripts --no-audit --no-fund \
    && npm cache clean --force

FROM deps AS install
COPY deploy/runtime-proof/verify.mjs ./
RUN --network=none node verify.mjs --packages-only \
    && ./node_modules/@opencode/cli-linux-x64/bin/opencode --version \
    && node node_modules/@anthropic-ai/claude-code/install.cjs \
    && npm_config_offline=true node node_modules/@opencode/cli/postinstall.mjs \
    && node verify.mjs

FROM node:24.21.0-bookworm-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6 AS runtime
COPY --from=install /opt/blaxsmith /opt/blaxsmith
ENV NODE_ENV=production PATH=/opt/blaxsmith/node_modules/.bin:${PATH} HOME=/tmp
WORKDIR /work
USER 10001:10001
CMD ["node", "/opt/blaxsmith/verify.mjs"]
