# forge — Setup Guide

This guide covers three deployment paths:

- **[Eval mode](#eval-mode-local-zero-dependencies)** — local process or Docker Compose, filesystem storage, no external dependencies.
- **[Local Kubernetes development](#local-kubernetes-development-kind)** — kind cluster on a local machine (including WSL2 on Windows), Helm-deployed, image loaded from the local Docker daemon.
- **[Production mode](#production-mode-kubernetes)** — persistent Kubernetes deployment backed by Postgres and S3-compatible object storage.

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
seeded automatically (see [Default repositories](#default-repositories)).

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

## Local Kubernetes development (kind)

Use this path to test the Helm chart and Kubernetes behaviour locally without
a cloud cluster. The workflow below is written for **WSL2 on Windows** but
works on any Linux or macOS machine with Docker.

### Prerequisites

Install these tools once:

| Tool | Install |
|------|---------|
| Docker Desktop (or Docker Engine) | https://docs.docker.com/get-docker/ |
| kind | `go install sigs.k8s.io/kind@latest` or the binary from https://kind.sigs.k8s.io |
| kubectl | https://kubernetes.io/docs/tasks/tools/ |
| helm 3.8+ | `curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \| bash` |

On WSL2, Docker Desktop's "Use the WSL 2 based engine" setting must be
enabled so that `docker` and `kind` share the same daemon.

### First-time cluster setup

```bash
# Create a single-node cluster
kind create cluster --name forge

# Build the image and load it into the cluster
docker build -t forge:local .
kind load docker-image forge:local --name forge

# Install via Helm
# storage.fs.storageClassName=standard is required for kind's default
# StorageClass (no dynamic provisioner is registered under a different name)
helm install forge deploy/helm/forge \
  --set image.tag=local \
  --set storage.fs.storageClassName=standard

# Wait for the pod to become ready
kubectl rollout status deployment/forge
```

### Port-forwarding

```bash
kubectl port-forward svc/forge 8080:8080 &
```

Verify:

```bash
curl http://localhost:8080/healthz   # → ok
```

**Port-forward stability note:** `kubectl port-forward` is a development tool,
not a stable tunnel. It holds a long-lived HTTP/2 connection to the Kubernetes
API server. On WSL2 this connection can drop silently after a Windows network
event (WiFi reconnect, VPN toggle, sleep/resume) or after the API server's
idle-connection timeout. The process stays alive in `ps` output but stops
forwarding — new requests return `curl` exit code 56 (connection reset) with
HTTP 000.

To avoid needing to restart it manually, use a restart loop:

```bash
while true; do
  kubectl port-forward svc/forge 8080:8080
  echo "port-forward exited, restarting in 2s..."
  sleep 2
done &
```

### Rebuilding after code changes

```bash
docker build -t forge:local .
kind load docker-image forge:local --name forge
kubectl rollout restart deployment/forge
kubectl rollout status deployment/forge
```

### Running tests against the cluster

Unit tests do not require the cluster:

```bash
go test ./...
```

The end-to-end smoke test builds the binary and starts a local process — it
does not use the cluster:

```bash
go build -o forge ./cmd/forge && bash test.sh
```

### Shutting down

```bash
pkill -f "kubectl port-forward"
kind delete cluster --name forge
```

---

## Working with CRAN packages

The CRAN handler supports hosted, proxy, and group repositories. The default
`cran-public` group merges `cran-hosted` (locally uploaded packages) with
`cran-proxy` (upstream CRAN). The examples below assume the cluster is running
and port-forwarded to `:8080`.

### Uploading a source package

```bash
curl -X PUT \
  http://localhost:8080/repository/cran-hosted/src/contrib/mypkg_1.0.0.tar.gz \
  --data-binary @mypkg_1.0.0.tar.gz
```

The server parses `DESCRIPTION` from the tarball, extracts `Package`,
`Version`, `License`, `Depends`, `Imports`, `Suggests`, and
`NeedsCompilation`, then updates the `PACKAGES`, `PACKAGES.gz`, and
`PACKAGES.rds` indices immediately.

### Uploading a Windows binary package

Windows R prefers a pre-built `.zip` over source. Build the binary on a
Windows machine:

```r
# In Windows R — produces mypkg_1.0.0.zip in the working directory.
# If zip is not on PATH, point R at the one bundled with Rtools:
Sys.setenv(R_ZIPCMD = "C:/rtools44/usr/bin/zip.exe")
install.packages("mypkg_1.0.0.tar.gz", repos = NULL,
                 type = "source", INSTALL_opts = "--build")
```

If `--build` fails to find `zip` (common on machines without Rtools), zip the
installed package directory manually using PowerShell:

```powershell
# Run in PowerShell — replace the R version and username as needed
Compress-Archive `
  -Path "C:\Users\<you>\AppData\Local\R\win-library\4.4\mypkg" `
  -DestinationPath "mypkg_1.0.0.zip" -Force
```

Upload from WSL2 (paths under `/mnt/c/` map to the Windows `C:\` drive):

```bash
curl -X PUT \
  "http://localhost:8080/repository/cran-hosted/bin/windows/contrib/4.4/mypkg_1.0.0.zip" \
  --data-binary @/mnt/c/Users/<you>/Documents/mypkg_1.0.0.zip
```

Or from Git Bash on Windows (use the Windows path directly):

```bash
curl -X PUT \
  "http://localhost:8080/repository/cran-hosted/bin/windows/contrib/4.4/mypkg_1.0.0.zip" \
  --data-binary @C:/Users/<you>/Documents/mypkg_1.0.0.zip
```

### Installing from R

Point R at the group repository so it sees both hosted and proxied packages:

```r
options(repos = c(CRAN = "http://localhost:8080/repository/cran-public/"))
install.packages("mypkg")
library(mypkg)
```

R's resolution order on Windows:

1. Checks `bin/windows/contrib/{R-version}/PACKAGES` for a binary `.zip`.
2. Falls back to `src/contrib/PACKAGES` for a source tarball.

On Linux, R only uses `src/contrib/`. Packages with `NeedsCompilation: no`
install from source on Windows without Rtools.

### Verifying the index

```bash
# Source index — check a specific package entry
curl -s http://localhost:8080/repository/cran-public/src/contrib/PACKAGES \
  | grep -A8 "^Package: mypkg"

# Windows binary index
curl -s "http://localhost:8080/repository/cran-hosted/bin/windows/contrib/4.4/PACKAGES"
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

## Declarative configuration (config-as-code)

Instead of relying on the hardcoded seed repos and managing configuration via
the admin UI, you can provide a `forge.config.json` file that forge reads on
boot and reconciles to. This is the recommended approach for GitOps deployments.

```bash
# Export current state as a starting point (secrets are blanked).
./forge -config-export -data ./data > forge.config.json

# Validate the file without making changes.
./forge -config-check -config forge.config.json -data ./data

# Start forge in config mode (seed repos are skipped).
./forge -config forge.config.json -data ./data
```

In Kubernetes, set `config.content` (inline JSON) or `config.existingConfigMap`
in the Helm values. The chart renders a ConfigMap, mounts it, appends
`-config /etc/forge/config.json` to the container args, and adds a
`checksum/config` pod annotation so a config change rolls the Deployment.

See [docs/runbooks/config-as-code.md](runbooks/config-as-code.md) for the full
schema reference, secret injection via `${ENV_VAR}`, prune semantics, and the
GitOps migration guide.

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

# kind (local dev)
pkill -f "kubectl port-forward"
kind delete cluster --name forge

# Docker Compose (also removes data volume)
docker compose down -v
```
