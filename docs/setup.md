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
| `-addr` | `:8080` | Listen address (env `FORGE_ADDR`) |
| `-data` | `./data` | Data directory (blobs + metadata) (env `FORGE_DATA`; the image sets `/data`) |
| `-auth` | off | Enable token authentication (see [Authentication](auth.md)) |
| `-drain-timeout` | `30s` | Graceful shutdown window for in-flight requests |
| `-log-format` | `json` | Log format: `json` or `text` |

### Option C — Plain `docker run`

The image runs as the distroless **nonroot user, uid/gid 65532**, and keeps its
data in `/data`. A named volume inherits the right ownership; a tmpfs or a bind
mount does not, so give it to that user:

```bash
docker run -d -p 8080:8080 --read-only --tmpfs /data:uid=65532,gid=65532 ghcr.io/gkroken/forge
# bind mount: sudo chown -R 65532:65532 ./forge-data && docker run -v "$PWD/forge-data:/data" ...
```

Arguments go straight to `forge` (e.g. `... ghcr.io/gkroken/forge -addr :80 -auth`);
the data directory stays `/data` because the image sets it through `FORGE_DATA`.

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

On Linux, R only uses `src/contrib/` — see *Linux binary packages* below for
how forge serves binaries there. Packages with `NeedsCompilation: no` install
from source on Windows without Rtools.

### Verifying the index

```bash
# Source index — check a specific package entry
curl -s http://localhost:8080/repository/cran-public/src/contrib/PACKAGES \
  | grep -A8 "^Package: mypkg"

# Windows binary index
curl -s "http://localhost:8080/repository/cran-hosted/bin/windows/contrib/4.4/PACKAGES"
```

### Linux binary packages

CRAN publishes no Linux binaries, so every R install on Linux compiles. forge
can serve them from builds you control: a **builder** (normally your CI, using
your runtime base image) uploads each package it compiles, and every later
install on the same platform — same distro release, R minor version and CPU —
gets the binary through the same group URL. Anything forge cannot match
exactly gets the source package, as before. forge never compiles or runs
package code; it checks each upload statically (layout, `Built:`, that it was
built from the source forge serves, and that its shared objects suit the
platform). Background and measurements: `WORKPLAN-CRAN-LINUX.md`,
`docs/notes/cran-linux-clients.md`.

**1. Create the binary repository and add it to the group.** It must refuse
overwrites (`noOverwrite`) and name the group whose source it builds from:

```yaml
# config file (or POST /api/v1/repos with the same fields)
repositories:
  - name: cran-linux
    format: cran
    kind: hosted
    noOverwrite: true
    sourceRepo: cran-public
    anonymousRead: true
  - name: cran-public
    format: cran
    kind: group
    members: [cran-hosted, cran-proxy, cran-linux]
```

**2. Register the platform.** One registration per builder image, keyed
`{distro}-{arch}/{rminor}`; it records the image digest uploads must report and
the shared libraries the image provides:

```bash
scripts/cran-linux-manifest.sh ghcr.io/acme/r-base:4.6.1 > platform.json
curl -X PUT -H "Authorization: Bearer $ADMIN" --data @platform.json \
  "$FORGE/api/v1/cran/platforms/$(jq -r .key platform.json)"
```

…or paste the JSON object under `cranPlatforms:` in the config file. When the
builder image changes, re-run the script and update the registration — uploads
from an image whose digest does not match are refused.

Registering is admin-only, but any authenticated token can read a registration
(`GET $FORGE/api/v1/cran/platforms/{key}`, or the list without a key), so a
builder or client can check `builderDigest` names the image it expects before
it uploads or installs binaries for that key.

**3. Give the builder a token** that can write only its own platform's tree:

```bash
curl -X POST -H "Authorization: Bearer $ADMIN" "$FORGE/api/v1/tokens" -d '{
  "description": "R builder noble-x86_64",
  "grants": [{"repo": "cran-linux", "actions": ["read", "write"],
              "selectors": ["bin/linux/noble-x86_64/**"]}]}'
```

**4. Run the helper after your build installs packages.** It uploads every
package the binary repository lacks for this platform (packages installed from
GitHub or local paths are skipped). It needs `curl` in the image (rocker
images lack it). The builder image's digest is not visible inside the
container, so pass it in:

```dockerfile
FROM ghcr.io/acme/r-base:4.6.1
ARG FORGE_URL FORGE_BUILDER_DIGEST
ENV R_LIBS_USER=/opt/rlib
COPY renv.lock cran-linux-upload.sh ./
RUN --mount=type=secret,id=forge_token \
    mkdir -p /opt/rlib && \
    Rscript -e 'renv::restore(library = "/opt/rlib", prompt = FALSE)' && \
    FORGE_TOKEN=$(cat /run/secrets/forge_token) ./cran-linux-upload.sh --lib /opt/rlib
```

Restore into a library of its own and point the helper at it (`--lib`), so it
offers only what this build installed. Packages that came with the base image
were often built elsewhere (rocker's littler, pak from its own repository);
forge refuses those as "not built from the source forge serves", which is
correct but noisy.

```bash
docker build --build-arg FORGE_URL=$FORGE \
  --build-arg FORGE_BUILDER_DIGEST=$(scripts/cran-linux-manifest.sh --digest ghcr.io/acme/r-base:4.6.1) \
  --secret id=forge_token,env=FORGE_TOKEN .
```

The digest is the image's config digest (`config.digest` in its registry
manifest), the same on every host. Do not use `docker image inspect -f
'{{.Id}}'`: on Docker's containerd image store, the default from Engine 29, that
is the manifest digest, and every upload would be refused 403 as built in
another image.

The first build of each package version compiles and uploads; every later one
installs the binary. A failed upload only warns (pass `--strict` to fail).

A builder of its own can read the upload's status: **201** stored; **409**
someone uploaded that binary first (done); **400** the request or the binary
itself is wrong, and sending it again will be refused again; **403** the key,
repository or builder digest does not allow it, until an admin changes that;
**413** larger than forge's upload limit (5 GiB); **408**, **502** and other **5xx** are
transient (a broken transfer, or the source could not be fetched), so retry.

**5. Point clients at the group's Linux URL:**

```r
options(repos = c(CRAN = "https://forge.example.com/repository/cran-public/__linux__/noble/latest"))
```

- **renv** and **pak** identify R's version and CPU themselves. renv records
  `…/cran-public/latest` in the lockfile and finds its way back to the binary
  URL at restore (forge answers renv's `/repository/__api__/status` probe).
- **install.packages** sends R's version only when `HTTPUserAgent` is set the
  way rocker images set it; stock R sends `libcurl/…` and gets source:

  ```r
  options(HTTPUserAgent = sprintf("R/%s R (%s)", getRversion(),
    paste(getRversion(), R.version["platform"], R.version["arch"], R.version["os"])))
  ```

Check what a client gets — `X-Forge-Package-Type` says `binary` or `source`:

```bash
curl -sI -A "R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)" \
  "$FORGE/repository/cran-public/__linux__/noble/latest/src/contrib/jsonlite_2.0.0.tar.gz" \
  | grep -i x-forge-package-type
```

**Removing a bad binary.** renv does not fall back to source when a binary
fails to load, so a broken binary blocks every client on its platform until it
is removed. Removal is one audited call; the next build recompiles and uploads
it again:

```bash
curl -X DELETE -H "Authorization: Bearer $ADMIN" \
  "$FORGE/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/jsonlite_2.0.0.tar.gz"
```

The Browse detail pane lists each version's Linux builds with their provenance
(uploader, builder image, source digest, `LinkingTo` versions) and system
requirements (the shared libraries they need).

**Eviction** is opt-in: attach a cleanup policy with `lastDownloadedDays` to
`cran-linux`. Each binary is judged by its own downloads (including those
served through the group), so an unused platform's build is evicted without
touching another platform's or the source; `keepVersions` counts per platform.

**Limits.** A client on another R minor version, another CPU or an
unregistered distro gets source. A lockfile is portable; the `__linux__` URL in
`options(repos)` names a distro. Uploads, rejections (with the reason) and
removals are audited and raise webhooks.

---

## Production mode (Kubernetes)

Production mode uses **Postgres** for metadata and an **S3-compatible object
store** for artifact blobs. The application tier is stateless — pods are
disposable and horizontally scalable.

### Prerequisites

- A Kubernetes cluster (1.25+)
- `helm` 3.8+ and `kubectl` configured for the target cluster
- Either: a Postgres instance and an S3-compatible object store (AWS S3, GCP
  Cloud Storage, Cloudflare R2, Ceph RGW, SeaweedFS, Garage, etc.)
- Or: nothing — `forge-stack` bundles both (see below)

### Option A — forge-stack (all-in-one, recommended for getting started)

`forge-stack` bundles forge + Postgres (the official image) + SeaweedFS (an
S3-compatible object store); both keep their data on persistent volumes. No external services needed. The
bundled store is for getting started: in production, point forge at the S3
service your organisation already runs (Option B). forge needs only basic S3 —
put (with multipart), get, stat, list, delete and bucket creation — so any
S3-compatible store works. MinIO was bundled until its community images were
removed from Docker Hub in September 2026; chart 0.2.0 replaced it, renaming
the `minio:` values to `seaweedfs:`. A 0.1.x install's bundled MinIO kept its
data in the container, not on a volume, so there is nothing to migrate from it.

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
kubectl -n forge port-forward svc/forge-stack 8080:8080
```

#### Where the images come from

Every image the charts deploy is pinned by digest and pulled from
`ghcr.io/gkroken`: forge's own, plus copies of the third-party ones listed in
`deploy/third-party-images.txt`. CI copies those byte for byte from Docker Hub
(same digest, every architecture), so an install never depends on an upstream
registry keeping an image (MinIO's vanished in 2026-09). Before it copies an
image, CI scans it with forge's image policy (no fixable HIGH or CRITICAL
findings), then signs the copy with cosign and attaches an SBOM. The policy's
exceptions live in `deploy/third-party-images.trivyignore.yaml`. Each one names
the file it applies to, gives a justification and expires, after which the gate
fails again.

For an air-gapped cluster or your own registry, copy the same digests and set
one value; image paths and digests stay the same:

```bash
DEST=registry.example.com/forge bash scripts/mirror-images.sh   # needs crane
crane copy ghcr.io/gkroken/forge:<tag> registry.example.com/forge/forge:<tag>
helm install forge-stack deploy/helm/forge-stack \
  --set global.imageRegistry=registry.example.com/forge ...
```

Verify a mirrored image's signature with:

```bash
cosign verify ghcr.io/gkroken/library/postgres@sha256:... \
  --certificate-identity-regexp '^https://github.com/gkroken/forge/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

#### Upgrading forge-stack 0.2.x → 0.3.0 (Postgres)

0.3.0 replaces the Bitnami Postgres subchart, whose images also left Docker
Hub, with a small StatefulSet on the official `postgres` image. The Service
name (`forge-stack-postgresql`) and the `postgresql.auth` values are the same,
so forge's `POSTGRES_DSN` does not change. `postgresql.primary.persistence.size`
and `postgresql.primary.resources` are now `postgresql.persistence.size` and
`postgresql.resources`.

The new StatefulSet starts on a **new, empty volume**. The old volume
(`data-forge-stack-postgresql-0`) is kept, but nothing reads it. To carry the
metadata over, dump it before upgrading and load it into the new database
before forge runs again:

```bash
kubectl -n forge scale deploy/forge-stack --replicas=0
kubectl -n forge exec forge-stack-postgresql-0 -- \
  sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U postgres -d forge' > forge.sql
helm upgrade forge-stack deploy/helm/forge-stack -n forge ...
kubectl -n forge scale deploy/forge-stack --replicas=0   # the upgrade restarted it
kubectl -n forge wait --for=condition=ready pod/forge-stack-postgres-0
# forge initialised the new database when it started; replace that with the dump
kubectl -n forge exec forge-stack-postgres-0 -- psql -U postgres \
  -c 'DROP DATABASE forge WITH (FORCE)' -c 'CREATE DATABASE forge'
kubectl -n forge exec -i forge-stack-postgres-0 -- \
  psql -U postgres -d forge -v ON_ERROR_STOP=1 < forge.sql
kubectl -n forge scale deploy/forge-stack --replicas=1
```

Blobs live in SeaweedFS and are unaffected. Once forge is serving, delete the
old volume: `kubectl -n forge delete pvc data-forge-stack-postgresql-0`.

### Option B — Standalone chart with external Postgres + S3

Use this when you already have managed Postgres and object storage.

```bash
helm install forge deploy/helm/forge \
  --namespace forge --create-namespace \
  --set storage.type=external \
  --set extraEnv.POSTGRES_DSN="postgres://forge:secret@pg-host:5432/forge?sslmode=require" \
  --set extraEnv.S3_ENDPOINT="https://s3.example.com" \
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

## OCI image scanning (Trivy, optional)

npm and Maven vulnerability scanning is built in (OSV.dev, no setup). Scanning
**OCI/Docker images** that forge hosts is **opt-in** and needs the external
[Trivy](https://aquasecurity.github.io/trivy/) scanner. It is **off by default** —
if you don't enable it, nothing below applies and forge runs exactly as before.

### How forge uses Trivy

forge shells out to the `trivy` binary (`trivy image …`) and parses its JSON into
the same findings store, Security UI, and download-policy gate as OSV. **forge does
not bundle Trivy** — you supply it. Two consequences:

- The binary must be **on forge's `PATH`** (it runs as an in-process subprocess, not
  a network service).
- The default container image is **distroless and does not include Trivy**. Setting
  the flag on the stock image will fail every scan with "executable file not found".
  In Kubernetes you must run forge from an image that *also* contains the trivy
  binary (see below).

### Flags / environment

| Flag | Env | Purpose |
|------|-----|---------|
| `-trivy-addr` | `TRIVY_ADDR` | forge's own registry host:port (e.g. `localhost:8080`). **Setting this enables OCI scanning.** |
| `-trivy-binary` | `TRIVY_BINARY` | path to the trivy binary (default `trivy`, looked up on `PATH`) |
| `-trivy-auth-token` | `TRIVY_AUTH_TOKEN` | forge API token Trivy uses to pull from an auth-enabled forge registry; empty = anonymous |

### Eval / local (binary mode)

```bash
# Install trivy (see https://aquasecurity.github.io/trivy/ for your platform), then:
trivy image --download-db-only      # pre-warm the vuln DB (~100 MB, cached in ~/.cache/trivy)

./forge -addr :8080 -data ./data -trivy-addr localhost:8080

# Push an image to the hosted OCI repo and scan it.
docker push localhost:8080/docker-hosted/myapp:1.0
curl -X POST http://localhost:8080/api/v1/repos/docker-hosted/scan   # 202; async
```

Findings then appear on the Browse detail pane, the Security column, the dashboard
tile, and **Security → Findings** — identical to npm/Maven. The download policy gate
(Off/Warn/Block) applies to OCI pulls too. Scans also run automatically on image push
and on the daily re-scan tick.

### Kubernetes

Because the stock image has no trivy, build a thin derived image and deploy that:

```dockerfile
# Dockerfile.scanning
FROM ghcr.io/<org>/forge:latest
COPY --from=aquasec/trivy:0.72.0 /usr/local/bin/trivy /usr/local/bin/trivy
```

```bash
helm upgrade --install forge deploy/helm/forge \
  --set image.repository=<your-registry>/forge-scanning \
  --set extraEnv.TRIVY_ADDR="localhost:8080"
```

Operational notes:

- **Vuln DB:** Trivy auto-downloads its DB (~100 MB) on first scan into
  `~/.cache/trivy`. Mount a writable volume for it (the runtime is
  `readOnlyRootFilesystem`) and optionally pre-warm/refresh it with a `CronJob`
  running `trivy image --download-db-only` so scans never block on a cold DB.
- **Egress:** scanning images referenced from *external* registries (Helm charts,
  later) needs pod egress to those registries; scanning forge-hosted images does not.
- Scanning is async and best-effort — a scan never blocks or fails a push or pull.

> Validated against **Trivy v0.72.0**. Trivy's JSON output is parsed by
> `internal/trivy`; if a future Trivy release changes that schema, update
> `parseOutput` there. Pin the trivy version in your derived image.

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
