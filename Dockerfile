FROM golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS api-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY db ./db
COPY gen/go ./gen/go
COPY internal ./internal
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o /out/blaxsmith ./cmd/blaxsmith

FROM scratch AS api
COPY --from=api-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=api-build /out/blaxsmith /blaxsmith
USER 65532:65532
EXPOSE 8001
ENTRYPOINT ["/blaxsmith"]
CMD ["serve"]

FROM node:24.21.0-alpine3.24@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend ./
RUN npm run build

FROM caddy:2.11.4-alpine@sha256:de23def33b17fb5d1290b0f6c2add1d70780e52341896c00a4c8a2a2fe9d355e AS web
# The upstream binary has a low-port file capability; copy strips it because we only listen on 8080.
RUN cp /usr/bin/caddy /usr/bin/caddy-unprivileged && mv /usr/bin/caddy-unprivileged /usr/bin/caddy
COPY deploy/images/Caddyfile /etc/caddy/Caddyfile
COPY --from=web-build /src/frontend/dist /srv
ENV XDG_CONFIG_HOME=/tmp/caddy-config XDG_DATA_HOME=/tmp/caddy-data
USER 65532:65532
EXPOSE 8080
