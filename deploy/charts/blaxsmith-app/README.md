# Direct-TLS application chart

This chart stages the authenticated API and built web workspace from one
`Dockerfile` `app` image. It creates a ClusterIP HTTPS Service and no ingress.
The application checks PostgreSQL on `/healthz`, serves `/livez` independently,
and applies/verifies migrations before binding. No AX connector or agent
workers are installed by this chart.

Supply an immutable `repository@sha256:<manifest digest>` app image and three
existing namespace Secrets: a `kubernetes.io/tls` Secret with `tls.crt` and
`tls.key`; a signer Secret with a raw 32-byte Ed25519 seed under `seed`; and a
database Secret with a PostgreSQL URL under `url`. The certificate must match
the exact `origin` value. PostgreSQL requires verified TLS by default. The
chart projects TLS and signer material as group-readable, non-world-readable
files for UID/GID 65532; it does not create, log, or rotate those secrets.

```sh
helm upgrade --install app deploy/charts/blaxsmith-app \
  --namespace blaxsmith --create-namespace \
  --set-string image=REGISTRY/blaxsmith-app@sha256:IMAGE_DIGEST \
  --set-string origin=https://app.example.com \
  --set-string tlsSecretName=app-tls \
  --set-string signerSecretName=app-signer \
  --set-string databaseSecretName=app-database \
  --wait
```

Run `deploy/charts/blaxsmith-app/test.sh` to lint and render the chart. The
app image is built in packaging CI. This is a staging foundation, not a
real-user production installation: no external L4 exposure or trusted client
address design, key-rotation rollout, RBAC-protected project APIs, or AX
connector is packaged yet. The plaintext public preview remains a separate
chart; do not expose it as the authenticated product.
