# forge — Setup Guide

This guide covers two deployment paths: **eval mode** for local evaluation
with no external dependencies, and **production mode** for a persistent
Kubernetes deployment backed by Postgres and S3-compatible object storage.

---

## Eval mode (local, zero dependencies)

Eval mode runs forge with filesystem storage. No Postgres, no S3, no
Kubernetes required. All data is stored under a local directory.

### Option A — Docker Compose (recommended)

```bash
git clone https://github.com/gkroken/forge
cd forge
docker compose up -d --build --wait
```

forge is now running at **http://localhost:8080**. Default repositories are
seeded automatically (see [Repositories](#default-repositories)).

To stop and remove data:

```bash
docker compose down -v
```

### Option B — Static binary

```bash
git clone https://github.com/gkroken/forge
cd forge
go build -o forge ./cmd/forge
./forge -addr :8080 -data ./data
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:8080` | Listen address |
| `-data` | `./data` | Data directory (blobs + metadata) |
| `-auth` | off | Enable token authentication (see [Authentication](auth.md)) |
| `-drain-timeout` | `30s` | Graceful shutdown window for in-flight requests |
| `-log-format` | `json` | Log format: `json` or `text` |

### Healthcheck

```bash
curl http://localhost:8080/healthz   # → ok
curl http://localhost:8080/readyz    # → ok
```

---

## Production mode (Kubernetes)

Production mode uses **Postgres** for metadata and an **S3-compatible object
store** for artifact blobs. The application tier is stateless — pods are
disposable and horizontally scalable.

### Prerequisites

- A Kubernetes cluster (1.25+)
- `helm` 3.8+ and `kubectl` configured for the target cluster
- Either: a Postgres instance and an S3-compatible object store (MinIO, AWS
  S3, GCP Cloud Storage, etc.)
- Or: nothing — `forge-stack` bundles both (see below)

### Option A — forge-stack (all-in-one, recommended for getting started)

`forge-stack` bundles forge + Bitnami Postgres + MinIO as Helm sub-charts.
No external services needed.

```bash
# Pull in chart dependencies
helm dependency update deploy/helm/forge-stack/

# Install
helm install forge-stack deploy/helm/forge-stack \
  --namespace forge --create-namespace \
  --wait --timeout 5m
```

Check the status:

```bash
kubectl -n forge get pods
kubectl -n forge get svc
```

Port-forward to access locally:

```bash
kubectl -n forge port-forward svc/forge-stack-forge 8080:8080
```

### Option B — Standalone chart with external Postgres + S3

Use this when you already have managed Postgres and object storage.

```bash
helm install forge deploy/helm/forge \
  --namespace forge --create-namespace \
  --set storage.type=external \
  --set extraEnv.POSTGRES_DSN="postgres://forge:secret@pg-host:5432/forge?sslmode=require" \
  --set extraEnv.S3_ENDPOINT="https://minio.example.com" \
  --set extraEnv.S3_BUCKET="forge-artifacts" \
  --set extraEnv.S3_ACCESS_KEY="access-key" \
  --set extraEnv.S3_SECRET_KEY="secret-key" \
  --wait
```

For sensitive values (S3 credentials, DSN passwords), use a pre-created
Secret instead of `--set`:

```yaml
# secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: forge-backend-credentials
  namespace: forge
stringData:
  POSTGRES_DSN: "postgres://forge:secret@pg-host:5432/forge?sslmode=require"
  S3_SECRET_KEY: "secret-key"
```

```bash
kubectl apply -f secret.yaml
helm install forge deploy/helm/forge \
  --set storage.type=external \
  --set extraEnvFrom[0].secretRef.name=forge-backend-credentials \
  ...
```

### Enabling high availability

For HA, use external storage (`storage.type=external`) and enable the HPA and
PDB. The app tier is stateless so pods can be scaled freely.

```bash
helm upgrade forge deploy/helm/forge \
  --set storage.type=external \
  --set replicaCount=2 \
  --set autoscaling.enabled=true \
  --set autoscaling.minReplicas=2 \
  --set autoscaling.maxReplicas=6 \
  --set podDisruptionBudget.enabled=true \
  --set podDisruptionBudget.minAvailable=1
```

> **Note:** `autoscaling.enabled=true` requires `storage.type=external`. FS
> storage uses a PVC in `ReadWriteOnce` mode which cannot be shared across
> pods.

### Configuring an Ingress

```bash
helm upgrade forge deploy/helm/forge \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set "ingress.hosts[0].host=forge.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.hosts[0].paths[0].pathType=Prefix"
```

For TLS, add a `tls` section to the ingress values. See
`deploy/helm/forge/values.yaml` for the full schema.

### GitOps

Example manifests for Argo CD and Flux are provided in `deploy/gitops/`:

```
deploy/gitops/argocd-application.yaml   # Argo CD Application
deploy/gitops/flux-helmrelease.yaml     # Flux HelmRepository + HelmRelease
```

Edit the `<EDIT>` placeholders (org name, target namespace, chart version)
before applying.

---

## Default repositories

forge seeds the following repositories on first start:

| Name | Format | Kind | Default access |
|------|--------|------|----------------|
| `maven-hosted` | Maven | hosted | anonymous read (eval), token required (auth) |
| `npm-hosted` | npm | hosted | anonymous read (eval), token required (auth) |
| `helm-hosted` | Helm | hosted | anonymous read (eval), token required (auth) |
| `cran-hosted` | CRAN | hosted | anonymous read (eval), token required (auth) |
| `docker-hosted` | OCI | hosted | anonymous read (eval), token required (auth) |
| `maven-central` | Maven | proxy → Maven Central | anonymous read |
| `npm-proxy` | npm | proxy → registry.npmjs.org | anonymous read |
| `cran-proxy` | CRAN | proxy → cran.r-project.org | anonymous read |
| `maven-public` | Maven | group (hosted + central) | anonymous read |
| `npm-public` | npm | group (hosted + proxy) | anonymous read |
| `helm-public` | Helm | group (hosted) | anonymous read |
| `cran-public` | CRAN | group (hosted + proxy) | anonymous read |

Repositories are persisted to the meta store and survive restarts. You can
create additional repositories via the Admin API or the web UI at `/ui/`.

---

## Upgrading

```bash
# Pull the latest chart
helm repo update  # if using a Helm repo
# or: git pull (if running from source)

helm upgrade forge deploy/helm/forge --reuse-values
```

Schema migrations run automatically on startup. Rollback by re-deploying the
previous version — `migrate.Down` is tested but destructive (wipes all
metadata). Only use it deliberately.

---

## Uninstalling

```bash
# Kubernetes
helm uninstall forge -n forge
kubectl delete namespace forge

# Docker Compose (also removes data volume)
docker compose down -v
```
