ARG TOOL_WORKER_IMAGE
FROM ${TOOL_WORKER_IMAGE}
COPY --chmod=0555 deploy/dev/model-probe-cli /usr/local/bin/blaxsmith-model-probe-cli
