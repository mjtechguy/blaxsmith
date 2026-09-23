# Read-only catalog preview chart

This chart runs the current public tool-release catalog and built frontend in
one Kubernetes pod. The Go API keeps its deliberate `127.0.0.1:8001` bind;
Caddy serves the UI on port 8080 and proxies `/api` inside the pod. The chart
creates a ClusterIP Service only. It does not expose login, database-backed
product records, credentials, AX agents, or a connector.

Build both images from the repository root with the pinned Go 1.27.1, Node
24.21.0, and Caddy 2.11.4 base-image digests in the `Dockerfile`:

```sh
docker build --target api -t REGISTRY/blaxsmith-api:VERSION .
docker build --target web -t REGISTRY/blaxsmith-web:VERSION .
docker push REGISTRY/blaxsmith-api:VERSION
docker push REGISTRY/blaxsmith-web:VERSION
```

Replace `REGISTRY` and `VERSION` with your registry and release. Record the
immutable `repository@sha256:<manifest digest>` for each pushed image using
`docker buildx imagetools inspect`. Ensure the image architecture matches the
cluster nodes; publish a multi-platform image index when serving mixed node
architectures. Install using **those digest references**, not the mutable
build tags:

```sh
helm upgrade --install preview deploy/charts/blaxsmith-preview \
  --namespace blaxsmith-preview --create-namespace \
  --set-string apiImage=REGISTRY/blaxsmith-api@sha256:API_DIGEST \
  --set-string webImage=REGISTRY/blaxsmith-web@sha256:WEB_DIGEST \
  --wait
kubectl -n blaxsmith-preview port-forward svc/preview-preview 8080:80
```

Open `http://127.0.0.1:8080/tools`. Both image values are required; the chart
rejects mutable tags. A private registry may use `imagePullSecrets` to name
Secrets created separately in the release namespace. The pods receive no
Kubernetes API token. Both containers run as UID/GID 65532, drop capabilities,
and use read-only root filesystems. Only Caddy receives a 16 MiB temporary
volume. Readiness checks that the web server responds and the loopback API
port is open; the API process is restarted by Kubernetes if it exits. These
probes do not assert publisher availability, database readiness, or full
product health.

The catalog makes outbound HTTPS requests to `registry.npmjs.org` and needs
DNS and public CA trust. Its cache is per replica, so extra replicas multiply
publisher traffic. This preview has no PostgreSQL requirement or Secret
mount. A future authenticated application needs its own deployment values,
TLS/ingress and signing-key custody, database readiness, and connector chart;
do not repurpose this public chart as that release.

Run `deploy/charts/blaxsmith-preview/test.sh` to lint the chart, render its
security/probe settings, and verify mutable image tags are rejected. Run
`deploy/charts/blaxsmith-preview/smoke.sh` from the repository root to build
both images and prove the UI and internal API proxy under restricted local
containers. CI runs both checks. This chart is not yet validated as a
production installation on k3s.
