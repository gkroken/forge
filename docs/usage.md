# forge — Client Usage Guide

This guide shows how to configure each supported package manager to publish
to and install from forge. All examples assume forge is running at
`http://localhost:8080`. Substitute your actual host and port.

For authentication setup, see [Authentication](auth.md). Examples below use
`$TOKEN` for a token variable — replace with your actual token.

---

## Repository URL reference

```
http://localhost:8080/repository/{repo-name}/
```

| Repository | URL |
|------------|-----|
| Maven hosted | `http://localhost:8080/repository/maven-hosted/` |
| Maven proxy (Central) | `http://localhost:8080/repository/maven-central/` |
| Maven group (hosted + central) | `http://localhost:8080/repository/maven-public/` |
| npm hosted | `http://localhost:8080/repository/npm-hosted/` |
| npm proxy | `http://localhost:8080/repository/npm-proxy/` |
| npm group | `http://localhost:8080/repository/npm-public/` |
| Helm hosted | `http://localhost:8080/repository/helm-hosted/` |
| Helm group | `http://localhost:8080/repository/helm-public/` |
| CRAN hosted | `http://localhost:8080/repository/cran-hosted/` |
| CRAN proxy | `http://localhost:8080/repository/cran-proxy/` |
| CRAN group | `http://localhost:8080/repository/cran-public/` |
| OCI (Docker) registry | `localhost:8080` (OCI uses host:port, not /repository/) |

---

## Maven

### Resolving dependencies (Maven)

Add forge to your `pom.xml`:

```xml
<repositories>
  <repository>
    <id>forge</id>
    <url>http://localhost:8080/repository/maven-public/</url>
  </repository>
</repositories>
```

For snapshot resolution, add:

```xml
<repository>
  <id>forge-snapshots</id>
  <url>http://localhost:8080/repository/maven-hosted/</url>
  <snapshots><enabled>true</enabled></snapshots>
</repository>
```

### Deploying artifacts (Maven)

Add distribution management to `pom.xml`:

```xml
<distributionManagement>
  <repository>
    <id>forge</id>
    <url>http://localhost:8080/repository/maven-hosted/</url>
  </repository>
  <snapshotRepository>
    <id>forge</id>
    <url>http://localhost:8080/repository/maven-hosted/</url>
  </snapshotRepository>
</distributionManagement>
```

Add credentials to `~/.m2/settings.xml`:

```xml
<settings>
  <servers>
    <server>
      <id>forge</id>
      <username>ignored</username>
      <password>forge_your_token_here</password>
    </server>
  </servers>
</settings>
```

If forge is running over plain HTTP (no TLS), add this to `settings.xml` to
disable Maven's HTTP-blocker (Maven 3.8+):

```xml
<settings>
  <mirrors>
    <!-- Empty mirrors section overrides the built-in HTTP-blocker. -->
  </mirrors>
</settings>
```

Deploy:

```bash
mvn deploy
```

### Resolving and deploying (Gradle)

In `build.gradle` (Groovy DSL):

```groovy
repositories {
    maven {
        url = "http://localhost:8080/repository/maven-public/"
        allowInsecureProtocol = true  // only needed for plain HTTP
        credentials {
            username = "ignored"
            password = System.getenv("FORGE_TOKEN") ?: ""
        }
    }
}

// For publishing:
plugins {
    id 'maven-publish'
}

publishing {
    repositories {
        maven {
            name = "forge"
            url = "http://localhost:8080/repository/maven-hosted/"
            allowInsecureProtocol = true
            credentials {
                username = "ignored"
                password = System.getenv("FORGE_TOKEN") ?: ""
            }
        }
    }
}
```

```bash
export FORGE_TOKEN=forge_your_token_here
gradle publish
```

---

## npm

### Installing packages

Point npm at forge's npm proxy or group:

```bash
npm install lodash --registry http://localhost:8080/repository/npm-public/
```

To set forge as the default registry for a project, add to `.npmrc`:

```ini
registry=http://localhost:8080/repository/npm-public/
```

### Publishing packages

```bash
# .npmrc — configure auth for the hosted registry
# The key is the registry URL without the protocol prefix
//localhost:8080/repository/npm-hosted/:_authToken=forge_your_token_here
```

```bash
npm publish --registry http://localhost:8080/repository/npm-hosted/
```

### pnpm

```bash
# Install
pnpm add lodash --registry http://localhost:8080/repository/npm-public/

# Publish — configure auth in .npmrc (same as npm above)
pnpm publish --registry http://localhost:8080/repository/npm-hosted/
```

### yarn (v1 / classic)

yarn reads `.npmrc` for auth. Add the same `_authToken` line as npm above, then:

```bash
# Install
yarn add lodash --registry http://localhost:8080/repository/npm-public/

# Publish
yarn publish --registry http://localhost:8080/repository/npm-hosted/ \
  --no-git-tag-version --non-interactive
```

### Scoped packages

```ini
# .npmrc
@myorg:registry=http://localhost:8080/repository/npm-hosted/
//localhost:8080/repository/npm-hosted/:_authToken=forge_your_token_here
```

```bash
npm install @myorg/mypackage
npm publish  # publishes to npm-hosted because of the @myorg scope
```

---

## Helm

### Adding a Helm repository (classic mode)

```bash
helm repo add forge http://localhost:8080/repository/helm-hosted/
helm repo update
helm search repo forge/
```

### Pulling a chart

```bash
helm pull forge/mychart --version 1.0.0
helm install myrelease forge/mychart --version 1.0.0
```

### Pushing a chart (classic mode)

forge implements the ChartMuseum-compatible API:

```bash
helm package ./mychart
curl -sf -X POST \
  -H "Content-Type: application/gzip" \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @mychart-1.0.0.tgz \
  http://localhost:8080/repository/helm-hosted/api/charts
```

### OCI mode (`oci://`)

Helm 3.8+ supports OCI registries. forge's OCI handler serves Helm charts at
the same `host:port` as the Docker registry.

```bash
# Push
helm package ./mychart
helm push mychart-1.0.0.tgz oci://localhost:8080/docker-hosted --plain-http

# Pull
helm pull oci://localhost:8080/docker-hosted/mychart --version 1.0.0 --plain-http

# Install directly
helm install myrelease oci://localhost:8080/docker-hosted/mychart \
  --version 1.0.0 --plain-http
```

For authenticated registries, log in first:

```bash
helm registry login localhost:8080 \
  --username ignored \
  --password $TOKEN \
  --insecure-skip-tls-verify
```

Then push/pull without `--plain-http`:

```bash
helm push mychart-1.0.0.tgz oci://localhost:8080/docker-hosted
```

---

## CRAN (R packages)

### Installing source packages

Set forge as your CRAN mirror in R:

```r
options(repos = c(CRAN = "http://localhost:8080/repository/cran-public/"))
install.packages("mypackage")
```

Or per-install:

```r
install.packages("mypackage",
  repos = "http://localhost:8080/repository/cran-public/",
  type = "source")
```

For project-level configuration, add to `.Rprofile`:

```r
options(repos = c(CRAN = "http://localhost:8080/repository/cran-public/"))
```

### renv

In your `renv.lock`-managed project:

```r
# Set the forge repo before calling renv::restore() or renv::install()
options(repos = c(CRAN = "http://localhost:8080/repository/cran-public/"))
renv::install("mypackage")
```

Or configure in `renv/settings.json`:

```json
{
  "r.repos": [
    { "name": "CRAN", "url": "http://localhost:8080/repository/cran-public/" }
  ]
}
```

### pak

```r
options(repos = c(CRAN = "http://localhost:8080/repository/cran-public/"))
pak::pkg_install("mypackage")
```

### Publishing a source package

```bash
# Build the tarball
R CMD build ./mypackage                # produces mypackage_1.0.0.tar.gz

# Upload to forge
curl -sf -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @mypackage_1.0.0.tar.gz \
  http://localhost:8080/repository/cran-hosted/src/contrib/mypackage_1.0.0.tar.gz
```

The PACKAGES, PACKAGES.gz, and PACKAGES.rds indexes are regenerated
automatically on each upload.

### Publishing binary packages

forge supports Windows (`.zip`) and macOS (`.tgz`) binary packages under
per-platform paths:

```bash
# Windows binary (built on Windows with R CMD INSTALL --build)
curl -sf -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @mypackage_1.0.0.zip \
  "http://localhost:8080/repository/cran-hosted/bin/windows/contrib/4.4/mypackage_1.0.0.zip"

# macOS ARM64 binary
curl -sf -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @mypackage_1.0.0.tgz \
  "http://localhost:8080/repository/cran-hosted/bin/macosx/big-sur-arm64/contrib/4.4/mypackage_1.0.0.tgz"

# macOS x86_64 binary
curl -sf -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  --data-binary @mypackage_1.0.0.tgz \
  "http://localhost:8080/repository/cran-hosted/bin/macosx/x86_64/contrib/4.4/mypackage_1.0.0.tgz"
```

R automatically selects the correct binary path for its platform when you
call `install.packages(..., type = "binary")`.

---

## OCI / Docker registry

forge implements the OCI Distribution Spec v1.0. The registry is mounted at
the host root (not under `/repository/`), and the first path component is the
**repository name**.

```
localhost:8080/{repo-name}/{image-name}:{tag}
```

The default OCI repository is `docker-hosted`. Additional OCI repositories
can be created via the Admin API.

### Docker

```bash
# Login
docker login localhost:8080 -u ignored -p $TOKEN

# Push
docker tag myimage:latest localhost:8080/docker-hosted/myimage:latest
docker push localhost:8080/docker-hosted/myimage:latest

# Pull
docker pull localhost:8080/docker-hosted/myimage:latest
```

For plain HTTP (no TLS), configure Docker to allow the insecure registry.
Add to `/etc/docker/daemon.json`:

```json
{
  "insecure-registries": ["localhost:8080"]
}
```

Restart Docker after changing daemon.json.

### oras (OCI artifacts)

oras pushes and pulls arbitrary files as OCI artifacts — not just Docker
images.

```bash
# Push
oras push --plain-http \
  localhost:8080/docker-hosted/myartifact:v1 \
  myfile.bin

# Pull
oras pull --plain-http --output ./output \
  localhost:8080/docker-hosted/myartifact:v1
```

With auth:

```bash
oras push \
  --plain-http \
  --username ignored \
  --password $TOKEN \
  localhost:8080/docker-hosted/myartifact:v1 \
  myfile.bin
```

### crane (OCI utilities)

crane is useful for copying images between registries and inspecting manifests.

```bash
# Copy a public image into forge
crane copy --insecure \
  alpine:latest \
  localhost:8080/docker-hosted/alpine:latest

# Inspect a manifest
crane manifest --insecure localhost:8080/docker-hosted/alpine:latest

# Pull to a local tarball
crane pull --insecure \
  localhost:8080/docker-hosted/alpine:latest \
  ./alpine.tar
```

---

## Proxy repositories

Proxy repositories are read-through caches — on a cache miss, forge fetches
from the upstream registry, stores the result, and serves it. Subsequent
requests are served from forge's local cache until the TTL expires (default:
24 hours).

### Configuring proxy behaviour

Proxy TTL and upstream auth can be set per repository via the Admin API:

```bash
curl -s -X PUT http://localhost:8080/api/v1/repos/npm-proxy \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "npm-proxy",
    "format": "npm",
    "kind": "proxy",
    "upstream": "https://registry.npmjs.org",
    "proxyTTL": "48h",
    "proxyAuth": "Bearer upstream-token-if-needed",
    "anonymousRead": true
  }'
```

### Using a proxy repository

Configure clients the same way as for hosted repositories — just use the
proxy repository URL. Clients don't need to know whether they're hitting a
cache:

```bash
npm install lodash --registry http://localhost:8080/repository/npm-proxy/
```

---

## Group repositories

Group repositories merge results from multiple member repositories in priority
order. Use group repositories as the default endpoint for clients — they
transparently serve internal (hosted) packages and fall back to upstream
(proxy) packages.

The default groups are:

- `maven-public` → maven-hosted, then maven-central
- `npm-public` → npm-hosted, then npm-proxy
- `cran-public` → cran-hosted, then cran-proxy
- `helm-public` → helm-hosted

Creating a custom group:

```bash
curl -s -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "npm-team",
    "format": "npm",
    "kind": "group",
    "members": ["npm-internal", "npm-approved-third-party", "npm-proxy"],
    "anonymousRead": false
  }'
```

Members are tried in order. For packages present in multiple members, the
first member wins.

---

## Repository administration

### Creating a repository

```bash
# Hosted npm repository
curl -s -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "npm-internal",
    "format": "npm",
    "kind": "hosted",
    "anonymousRead": false
  }'

# Maven proxy for a private Artifactory
curl -s -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "maven-internal-proxy",
    "format": "maven",
    "kind": "proxy",
    "upstream": "https://artifactory.example.com/maven-local",
    "proxyAuth": "Bearer artifactory-token",
    "proxyTTL": "1h",
    "anonymousRead": false
  }'
```

### Listing repositories

```bash
curl -s http://localhost:8080/api/v1/repos | jq '.[].name'
```

### Deleting a repository

```bash
curl -s -X DELETE http://localhost:8080/api/v1/repos/npm-old \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Deleting a repository removes its configuration. Stored artifacts in the blob
store are not automatically deleted — clean them up manually from the data
directory or S3 bucket if needed.

---

## Web UI

The web UI is available at `http://localhost:8080/ui/`. It provides:

- **Browse** — list repositories and their contents
- **Search** — search for packages by name across all repositories
- **Admin** — create, update, and delete repositories

The UI is read-only for anonymous users (in eval mode). Admin pages call the
authenticated API — you will need a token with admin role to create or modify
repositories from the UI.
