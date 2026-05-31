# forge — multi-format artifact repository

A Nexus-style artifact repository supporting **Maven, npm, Helm, CRAN, and OCI**
in hosted, proxy, and group modes. Single static Go binary; zero external
dependencies for eval mode; Postgres + S3-compatible object store for production.

[![CI](https://github.com/gkroken/forge/actions/workflows/ci.yml/badge.svg)](https://github.com/gkroken/forge/actions/workflows/ci.yml)

---

## Quick start

**Eval (zero dependencies):**
```bash
docker compose up          # forge on :8080, data in a named volume
```

**Production (Kubernetes):**
```bash
# Bundles Postgres + MinIO — no external services needed
helm install forge-stack deploy/helm/forge-stack \
  --set forge.image.tag=latest \
  --wait
```

**Binary:**
```bash
go build -o forge ./cmd/forge
./forge -addr :8080 -data ./data
```

---

## Format support

| Format | Hosted | Proxy | Group | Clients verified |
|--------|:------:|:-----:|:-----:|-----------------|
| Maven  | ✅ | ✅ | ✅ | `mvn` 3.9, `gradle` 8.7 |
| npm    | ✅ | ✅ | ✅ | `npm`, `pnpm`, `yarn` |
| Helm   | ✅ | — | ✅ | `helm` 3.x (repo + `oci://`) |
| CRAN   | ✅ | ✅ | ✅ | `R` install.packages, `renv`, `pak` |
| OCI    | ✅ | — | — | `oras`, `crane`, `helm push oci://` |

All clients are exercised by the conformance suite against a live forge instance
(see `internal/conformance/`). The suite runs in CI on every push.

---

## Client usage

```bash
# npm — install through the proxy
npm install lodash --registry http://localhost:8080/repository/npm-proxy/

# npm — publish to hosted
npm publish --registry http://localhost:8080/repository/npm-hosted/

# Maven — resolve through the proxy (settings.xml)
#   <repository><url>http://localhost:8080/repository/maven-central/</url></repository>

# Maven — deploy to hosted (settings.xml + distributionManagement)
mvn deploy -DrepositoryId=forge -Durl=http://localhost:8080/repository/maven-hosted/

# Helm — classic repo
helm repo add forge http://localhost:8080/repository/helm-hosted/
helm push mychart-0.1.0.tgz oci://localhost:8080/docker-hosted   # OCI mode

# CRAN (R)
install.packages("pkg", repos="http://localhost:8080/repository/cran-hosted/")
# or set as your default mirror:
options(repos=c(forge="http://localhost:8080/repository/cran-public/"))

# OCI / Docker
oras push localhost:8080/docker-hosted/myimage:v1 artifact.bin
```

---

## Authentication

Enable token auth with `-auth`:
```bash
./forge -addr :8080 -data ./data -auth
# Prints a bootstrap admin token on first run. Store it — shown once.
```

Create scoped tokens via the API:
```bash
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"description":"ci-bot","grants":[{"repo":"npm-hosted","role":"write"}]}'
```

Roles: `read`, `write`, `admin`. Repos support `anonymousRead: true` for open
access (typical for install/resolve paths). Token auth is enforced as middleware
before every handler.

---

## Architecture

One shared spine; formats are plugins.

```
HTTP /repository/{repo-name}/{path...}
         │
    server.go         resolves repo name → Repository
         │             looks up Format → Handler
         ▼
  format.Registry     maps "maven"/"npm"/"helm"/"cran"/"oci" → Handler
         │
  Handler.Serve()     receives format.Context (repo, blob, meta, http client, sub-path)
         │
  ┌──────┴──────┐
blob.Store   meta.Store    interfaces; FS impl for eval, S3+Postgres for production
```

Adding a format = implementing one interface:

```go
type Handler interface {
    Format() string
    Serve(w http.ResponseWriter, r *http.Request, c *Context)
}
```

Nothing in routing, storage, or the repository model knows what Maven is.

**Storage backends:**

| | Eval | Production |
|--|------|-----------|
| Blob | filesystem (`data/blobs/`) | S3 / MinIO (set `S3_ENDPOINT`) |
| Meta | filesystem (`data/meta/`) | Postgres (set `POSTGRES_DSN`) |
| Queue | in-memory | Postgres (auto-selected when `POSTGRES_DSN` is set) |

---

## Kubernetes deployment

The `deploy/helm/forge` chart is the primary install path.

```bash
# Standalone chart — point at existing Postgres + S3
helm install forge deploy/helm/forge \
  --set extraEnv.POSTGRES_DSN="postgres://..." \
  --set extraEnv.S3_ENDPOINT="https://..." \
  --set extraEnv.S3_BUCKET="forge-artifacts"

# forge-stack — bundles Postgres (Bitnami) + MinIO, no external deps
helm install forge-stack deploy/helm/forge-stack
```

The chart includes: liveness/readiness/startup probes, HPA, PodDisruptionBudget,
graceful SIGTERM drain, non-root + read-only root FS, multi-arch image
(amd64/arm64), ConfigMap/Secret-based config, Prometheus `ServiceMonitor`.

GitOps examples: `deploy/gitops/argocd-application.yaml` and
`deploy/gitops/flux-helmrelease.yaml`.

---

## Repository layout

```
cmd/forge/              entrypoint — wires stores, registers handlers, seeds repos
internal/
  blob/                 blob.Store interface; FS + S3 implementations + contract suite
  meta/                 meta.Store interface; FS + Postgres implementations + contract suite
  auth/                 token store, per-repo RBAC, auth middleware (Bearer + npm Basic)
  proxy/                shared proxy fetcher: TTL, ETag revalidation, negative cache,
                        stale-on-error, retries, circuit breaker, upstream auth
  queue/                async index-regen queue; Mem (eval) + Postgres (HA) implementations
  indexer/              npm packument regen worker (idempotent, queue-driven)
  format/maven/         Maven 2: PUT/GET, checksum sidecars, maven-metadata.xml,
                        SNAPSHOT metadata, Gradle .module, parent-POM prefetch
  format/npm/           npm registry: publish, packument, tarballs, dist-tags,
                        deprecate, unpublish, audit bridge, login, group fan-out
  format/helm/          Helm repo: chart upload, index.yaml, chart API, OCI mode
  format/cran/          CRAN: DESCRIPTION parse, PACKAGES + PACKAGES.gz + PACKAGES.rds
  format/oci/           OCI Distribution Spec v1.0: blobs, manifests, tags, uploads
  server/               HTTP router, auth middleware wiring, admin API, browse/search UI
  obs/                  Prometheus metrics, structured logging, audit log
  conformance/          end-to-end conformance tests (real clients in Docker containers)
deploy/
  helm/forge/           production Helm chart
  helm/forge-stack/     all-in-one chart (forge + Postgres + MinIO)
  gitops/               ArgoCD Application + Flux HelmRelease examples
  terraform/            AWS + GCP modules for cloud-managed Postgres/S3 (post-GA)
docs/
  runbooks/             operations runbooks (backup, incident response, token mgmt, …)
  security/             threat model, pen test scope
load/
  smoke.js              k6 load test: metadata GET p99 < 50ms, 50 concurrent publishes
  soak.js               24h soak script (run manually pre-release)
```

---

## CI

Every push runs: lint (`go vet`, `helm lint`, `terraform validate`), unit tests
with `-race`, coverage gate (overall ≥75%, core packages ≥85%), integration tests
(Postgres + MinIO via testcontainers), conformance tests (all clients × formats),
SAST (`gosec`), dependency scan (`govulncheck`), and container scan (Trivy).

Nightly: full conformance matrix, DAST (ZAP baseline), k6 load test (SLO gate),
kind cluster install + conformance smoke, timed quickstart gate (< 10 min).

---

## Post-GA roadmap

- **OIDC / LDAP federation** — self-service token issuance via Keycloak/AD; token
  model covers current production workflows (anonymous install, CI service tokens).
- **CRAN binary trees** (`/bin/`) — per-OS pre-compiled packages; source packages
  work for current use cases.
- **Distributed tracing** — OpenTelemetry integration; Prometheus metrics +
  structured logs cover current operational needs.
- **Chaos suite** — automated pod-kill and S3/PG-blip recovery tests.
- **Cloud Terraform modules** — AWS + GCP modules exist; Azure and nightly
  apply/destroy validation are post-GA.
