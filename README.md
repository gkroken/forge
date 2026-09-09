# forge — multi-format artifact repository

A Nexus-style artifact repository supporting **Maven, npm, Helm, CRAN, PyPI, and OCI**
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
| PyPI   | ✅ | ✅ | ✅ | `pip` 24.x, `twine` 5.x |
| OCI    | ✅ | ✅ | ✅ | `oras`, `crane`, `helm push oci://` |

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

# PyPI (Python)
twine upload --repository-url http://localhost:8080/repository/pypi-hosted/ dist/*
pip install --index-url http://localhost:8080/repository/pypi-hosted/simple/ mypkg
# or through the group (internal packages shadow upstream ones of the same name):
pip install --index-url http://localhost:8080/repository/pypi-public/simple/ six

# OCI / Docker — a proxy caches upstream images (Docker Hub, ghcr.io, quay.io)
docker pull localhost:8080/repository/docker-public/library/alpine:latest

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
  -d '{"description":"ci-bot","grants":[{"repo":"npm-hosted","actions":["read","write"]}]}'
```

Grants carry explicit actions — `read`, `write`, `delete`, `admin` — per
repository (`"*"` = all), optionally narrowed by content selectors
(`"selectors":["com/acme/**"]`); `admin` on `*` is the global administrator,
`admin` on one repo delegates managing just that repo (see
[docs/auth.md](docs/auth.md)). Repos support `anonymousRead: true` for open
access (typical for install/resolve paths). Token auth is enforced as middleware
before every handler.

### Single sign-on (OIDC)

Forge authenticates operators against any OIDC identity provider — **Keycloak,
Entra / Azure AD, Okta, ADFS**. Active Directory integrates either through the IdP
(Keycloak federates on-prem AD over LDAP; Entra ID fronts Azure AD directly) or via
a **direct LDAP/AD bind** — see [Direct LDAP / Active Directory login](#direct-ldap--active-directory-login)
below. Both paths share the same group→role mapping.

Setting the issuer enables SSO. Every flag has a matching `OIDC_*` env var (the
flag overrides the env); prefer the env var for the client secret, since flags are
visible in `ps`:

```bash
./forge -addr :8080 -data ./data -auth \
  -oidc-issuer        https://keycloak.example.com/realms/forge \
  -oidc-client-id     forge \
  -oidc-client-secret "$OIDC_CLIENT_SECRET" \
  -oidc-redirect-url  https://forge.example.com/auth/oidc/callback \
  -oidc-group-mappings 'forge-admins:admin,developers:write,staff:read'
```

| Flag / env | Purpose |
|--|--|
| `-oidc-issuer` / `OIDC_ISSUER` | IdP issuer URL; **set this to enable SSO** |
| `-oidc-client-id` / `OIDC_CLIENT_ID` | OAuth client ID |
| `-oidc-client-secret` / `OIDC_CLIENT_SECRET` | OAuth client secret (prefer the env var) |
| `-oidc-redirect-url` / `OIDC_REDIRECT_URL` | `https://<host>/auth/oidc/callback` |
| `-oidc-groups-claim` / `OIDC_GROUPS_CLAIM` | ID-token claim with group membership (default `groups`) |
| `-oidc-group-mappings` / `OIDC_GROUP_MAPPINGS` | `group:role,…` mapping IdP groups onto roles |
| `OIDC_DEFAULT_GRANTS` | JSON grants applied when no group matches (default: `read` on `*`) |
| `-oidc-token-ttl` / `OIDC_TOKEN_TTL` | SSO session lifetime (default `8h`) |

**Group → role mapping** is the centerpiece: each IdP group maps to a base role
(`admin`/`write`/`read`); the highest matching role wins, and a login with no
matching group falls back to `OIDC_DEFAULT_GRANTS`. On first login an SSO user is
provisioned into the Users tab (no local password); disabling that user there
blocks future SSO logins. The live config and mapping table are shown read-only on
the **Access** admin page.

#### Keycloak quick-start (local)

```bash
docker run -p 8081:8080 \
  -e KEYCLOAK_ADMIN=admin -e KEYCLOAK_ADMIN_PASSWORD=admin \
  quay.io/keycloak/keycloak:latest start-dev
```

In the Keycloak admin console: create a realm (`forge`); create a confidential
client (`forge`) with redirect URI `http://localhost:8080/auth/oidc/callback` and
copy its secret; create a group `forge-admins` and a user in it; add a
**Group Membership** client mapper named `groups` (token claim name `groups`,
"Full group path" off) so the ID token carries the group. Then start forge with
`-oidc-issuer http://localhost:8081/realms/forge` and
`-oidc-group-mappings forge-admins:admin`, and sign in via "Sign in with SSO".

### Direct LDAP / Active Directory login

Users sign in on the normal login form with their **directory** username and
password. Forge verifies the credentials against the directory by *search-then-bind*
(bind as a service account → find the user → re-bind as that user) and, on success,
mints a **normal forge token** — the AD password never leaves the login POST and is
never written into `.npmrc`, `settings.xml`, or CI config. Group membership feeds the
same group→role mapping as OIDC. Anonymous-read repositories need no login at all.

Setting `-ldap-url` enables it. Every flag has a matching `LDAP_*` env var (prefer
the env var for the bind password, which is visible in `ps` as a flag):

```bash
./forge -addr :8080 -data ./data -auth \
  -ldap-url          ldaps://dc1.example.com:636,ldaps://dc2.example.com:636 \
  -ldap-bind-dn      'cn=forge-svc,ou=service,dc=example,dc=com' \
  -ldap-bind-password "$LDAP_BIND_PASSWORD" \
  -ldap-user-base-dn 'ou=people,dc=example,dc=com' \
  -ldap-user-filter  '(sAMAccountName=%s)' \
  -ldap-group-mappings 'forge-admins:admin,forge-devs:write,staff:read'
```

| Flag / env | Purpose |
|--|--|
| `-ldap-url` / `LDAP_URL` | Server URL(s), comma-separated for failover; **set this to enable LDAP login** |
| `-ldap-start-tls` / `LDAP_START_TLS` | Issue StartTLS on `ldap://` connections before binding |
| `-ldap-ca-cert` / `LDAP_CA_CERT` | PEM CA bundle for the TLS connection (default: system roots) |
| `-ldap-bind-dn` / `LDAP_BIND_DN` | Service-account DN for the search step (empty = anonymous search) |
| `-ldap-bind-password` / `LDAP_BIND_PASSWORD` | Service-account password (prefer the env var) |
| `-ldap-user-base-dn` / `LDAP_USER_BASE_DN` | Base DN for the user search |
| `-ldap-user-filter` / `LDAP_USER_FILTER` | User filter; `%s` = escaped login name (default `(uid=%s)`; AD: `(sAMAccountName=%s)`) |
| `-ldap-email-attr` / `LDAP_EMAIL_ATTR` | Attribute holding the user's email (default `mail`) |
| `-ldap-group-mode` / `LDAP_GROUP_MODE` | `memberof` (read the attribute off the user — AD default) or `search` |
| `-ldap-group-base-dn` / `LDAP_GROUP_BASE_DN` | Base DN for group search (mode `search`) |
| `-ldap-group-filter` / `LDAP_GROUP_FILTER` | Group filter; `%s` = escaped user DN (e.g. `(&(objectClass=groupOfNames)(member=%s))`) |
| `-ldap-group-attr` / `LDAP_GROUP_ATTR` | Attribute holding the group name (default `cn`) |
| `-ldap-group-mappings` / `LDAP_GROUP_MAPPINGS` | `group:role,…` mapping directory groups onto roles |
| `LDAP_DEFAULT_GRANTS` | JSON grants applied when no group matches (default: `read` on `*`) |
| `-ldap-token-ttl` / `LDAP_TOKEN_TTL` | LDAP session lifetime (default `8h`) |

The connection is TLS-protected: use `ldaps://`, or `ldap://` with `-ldap-start-tls`.
`-ldap-insecure-skip-verify` disables certificate verification for dev only and logs a
loud warning. When `-config` mode declares an `ldap` block, that block wins over these
flags. Local users (if any) are tried before LDAP, so the bootstrap admin keeps working.
The live config and mapping table are shown read-only on the **Access** admin page.

#### OpenLDAP quick-start (local)

```bash
docker run -p 389:1389 \
  -e LDAP_ADMIN_USERNAME=admin -e LDAP_ADMIN_PASSWORD=adminpassword \
  -e LDAP_USERS=alice,bob -e LDAP_PASSWORDS=alicepw,bobpw \
  -e LDAP_ROOT=dc=example,dc=org \
  -e LDAP_ADMIN_DN=cn=admin,dc=example,dc=org \
  bitnami/openldap:latest
```

Bitnami seeds users under `ou=users,dc=example,dc=org` and a group `readers`
(`ou=users`, `groupOfNames`) containing them. Start forge with:

```bash
./forge -auth \
  -ldap-url          ldap://localhost:389 \
  -ldap-bind-dn      'cn=admin,dc=example,dc=org' \
  -ldap-bind-password adminpassword \
  -ldap-user-base-dn 'ou=users,dc=example,dc=org' \
  -ldap-user-filter  '(cn=%s)' \
  -ldap-group-mappings 'readers:write'
```

Then sign in on the login form as `alice` / `alicepw`. A repeatable end-to-end
validation harness (custom LDIF for `forge-admins`/`forge-devs`, curl-driven login
assertions) lives at [`scripts/ldap-validate.sh`](scripts/ldap-validate.sh).

---

## Webhooks

Register HTTP endpoints (Admin → Webhooks, or `POST /api/v1/webhooks`) to receive
events: `artifact.published`, `artifact.deleted`, `artifact.cached` (proxy cache
fill), and `cleanup.completed`. Delivery is durable (Postgres queue in production,
in-memory in eval) with bounded exponential-backoff retries that also honour a
`Retry-After` header on `429`/`503`.

Each delivery POSTs a JSON envelope and these headers:

| Header | Meaning |
|---|---|
| `X-Forge-Event` | event type, for routing without parsing the body |
| `X-Forge-Delivery` | stable delivery id — **identical across retries**; dedup on it |
| `X-Forge-Timestamp` | Unix seconds, signed alongside the body |
| `X-Forge-Signature` | `sha256=<hex>` HMAC-SHA256 over `"{timestamp}.{body}"` |

Verify a delivery by recomputing the signature over the **raw** request body and
rejecting timestamps outside a tolerance window (replay protection):

```python
import hmac, hashlib, time

def verify(secret, headers, raw_body, tolerance=300):
    ts = int(headers["X-Forge-Timestamp"])
    if abs(time.time() - ts) > tolerance:           # replay window
        return False
    mac = hmac.new(secret.encode(), f"{ts}.".encode() + raw_body, hashlib.sha256)
    return hmac.compare_digest("sha256=" + mac.hexdigest(), headers["X-Forge-Signature"])
```

The body is a flat envelope: `{"schemaVersion":2,"id":"<delivery-id>","type":...,
"repo":...,"format":...,"path":...,"timestamp":...,"data":{...}}`.

Target URLs are checked against an SSRF policy at registration **and** at dial time
(defeating DNS rebinding): loopback, link-local, private, and the
`169.254.169.254` cloud-metadata endpoint are refused. Set
`WEBHOOK_ALLOW_PRIVATE=1` for internal-only deployments where receivers live on a
private network. Recent delivery attempts (including dropped/dead-letter) are
visible per endpoint in the UI and at `GET /api/v1/webhooks/{id}/deliveries`;
`forge_webhook_deliveries_total{result}` counts outcomes for alerting.

---

## Architecture

One shared spine; formats are plugins.

```
HTTP /repository/{repo-name}/{path...}
         │
    server.go         resolves repo name → Repository
         │             looks up Format → Handler
         ▼
  format.Registry     maps "maven"/"npm"/"helm"/"cran"/"pypi"/"oci" → Handler
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
  format/pypi/          PyPI: twine upload, PEP 503 simple index, pypi.org proxy, group
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

## Vulnerability scanning & download policy

forge scans stored and proxied **npm** and **Maven** artifacts against
[OSV.dev](https://osv.dev) (built in, no setup) and surfaces findings on the
Browse detail pane, the per-repo Security column, the dashboard tile, and the
**Security → Findings** admin page. Hosted **OCI/Docker images** can also be
scanned via [Trivy](https://aquasecurity.github.io/trivy/) — opt-in and off by
default; it needs an operator-supplied trivy binary (see
[Setup → OCI image scanning](docs/setup.md#oci-image-scanning-trivy-optional)).
Other formats are labelled "not scanned" rather than shown a misleading green
(CRAN has no credible advisory source; Helm is tracked separately).

A **download policy** can then warn on — or block — vulnerable downloads.
Configure it under **Security → Policies**:

- A **global default** plus reusable **named policies** (e.g. "strict"), each
  with an enforcement mode (Off / Warn / Block), a severity **threshold**, a
  **fail-open** switch for never-scanned artifacts, and audited **suppressions**
  (silence a specific CVE/GHSA with a reason; who and when are recorded).
- Each repository's **Security tab** assigns a named policy or inherits the
  global default. Resolution is _named policy → global default → Off_. Policies
  are admin-set: enforcement is a security boundary, so consumers can't
  self-downgrade Block to Warn. Use **Preview impact** to see the blast radius
  (how many component@versions a policy would block) before enforcing.

Enforcement gates the artifact download itself (jar/tarball — never metadata or
checksums): **Block** returns `403` with an advisory link; **Warn** serves the
artifact but adds an `X-Forge-Vulnerabilities` response header. Both are audited
and emit a `policy.violation` webhook; blocks increment
`forge_downloads_blocked_total`.

> **Honest caveat — Warn is forge-side only.** A warning shows in the UI, the
> `X-Forge-Vulnerabilities` header, the audit log, and metrics, but package
> managers don't render it — `npm install` and Maven won't surface it. Only
> **Block** is visible to the client, as a failed download. Treat Warn as a
> governance signal, not an install-time message.

---

## Storage integrity verify

forge can prove a repository's storage is internally consistent — the tool to
run after a migration, a restore from backup, or any time storage is suspect.
`POST /api/v1/repos/{name}/verify` (or the **Verify now** button on the repo's
**Integrity** tab / the **Integrity** admin rollup) enqueues a **read-only**
pass on the shared worker; findings are reported, never repaired automatically.

Four finding kinds, checked per format by the format plugin itself:

- **missing** — a metadata record (npm version, Helm chart, CRAN package, OCI
  manifest reference, Maven SNAPSHOT record) points at a blob that is gone.
  Clients see 404s.
- **mismatch** — stored bytes no longer match their recorded expectation:
  Maven checksum sidecars, npm `dist.shasum`/`integrity`, Helm chart digests,
  and OCI content-addresses are re-computed from the blob; CRAN (which stores
  no digest) is validated via the tarball's gzip CRC.
- **orphan** — an object nothing owns: a blob absent from every index, a
  record whose content is gone, a stale OCI upload buffer.
- **drift** — a materialized index out of sync with its source records (npm's
  packument). `POST /api/v1/repos/{name}/reindex` rebuilds it.

Two modes: **full** (default) re-reads every blob that carries an expectation —
IO-heavy but linear, and the only way to catch silent corruption; **quick**
cross-references records and blobs without hashing. One report per repo is
persisted (re-runs replace it); the UI shows verdict, per-kind counts, bytes
read, and staleness. Repo-scoped admins can verify their own repos; the fleet
rollup at **Integrity** is global-admin. Proxy-cache findings are hygiene only —
caches re-fetch from upstream on demand. Group repos own no storage; verify
their members.

---

## Dependency-confusion protection

A build resolving an internal package name through a group (or straight from a
proxy) must never receive an attacker's same-named package from the public
registry. forge enforces two ownership signals, **on by default** for every
group and proxy repository:

- **Auto-derived ownership** — any name actually present in a hosted group
  member is pinned to hosted members in that group. The group's npm packument /
  Maven metadata / Helm index / CRAN PACKAGES carry only hosted versions of the
  name, and a version that exists only upstream is a 404, never an upstream
  fetch. Zero configuration; the classic Azure-Artifacts-style pinning.
- **Explicit namespace claims** — hosted repos declare what they own with
  selector patterns (`@acme/**`, `com/acme/**`, `left-pad`), closing the
  publish race: a claimed-but-not-yet-published name is refused with
  `403 {"error":"blocked by dependency-confusion protection", "claim":…,
  "claimedBy":…}` instead of falling through — on groups containing the
  claiming repo *and* on every same-format proxy repo, cached or not. Claims
  are set on the repo form / Settings tab or the `claims` field of the repo
  API, validated at write time.

Blocked requests land in the audit log (`dep-guard: blocked …`), fire the
`policy.violation` webhook (`kind=dependency-confusion`), and count in
`forge_depguard_blocked_total{repo}`. Opt out per group/proxy repo with the
**Dependency-confusion guard** toggle (`depConfusionGuard: false`) to
deliberately merge internal and public versions of the same names. OCI has no
group support yet and is not guarded. `scripts/depguard-validate.sh` proves
the matrix live against registry.npmjs.org and Maven Central.

---

## Storage quotas & soft-delete

Every **hosted** repository can carry a storage quota (**Storage quota (GB)** on
the repo form, or `quotaGB` in the repo API). Once usage reaches the quota, the
next publish is refused in the request spine — before the format handler runs —
with `507 Insufficient Storage` and a JSON body naming the used and quota bytes.
The refusal is recorded in the audit log (`quota: blocked write …`), fires the
`policy.violation` webhook (`kind=quota`), and counts in
`forge_quota_blocked_total{repo}`; `forge_repo_quota_used_ratio{repo}` gauges
current fill. Only hosted publishes count — **proxy cache-fills are never
blocked** (refusing to cache a transitive dependency would break a build; proxy
growth is bounded by cache eviction instead). Accounting is a documented *soft*
limit: usage comes from a periodic blob walk (re-run after every write) plus an
in-flight byte delta, so a burst of concurrent uploads can overrun the quota
slightly before the next walk reconciles it — exact within one walk cycle, with
no write-path ledger.

Deleting a version through the admin **Delete** button (or `DELETE
/api/v1/repos/{name}/component`) is a **soft delete**: the artifact's blobs are
moved to a reserved `_trash/` key space and the metadata a hard delete would
have removed is captured into a tombstone. Because trash lives outside every
repo's key space, **deleting frees quota immediately** and trashed bytes never
show up as integrity orphans; disk is reclaimed only on purge. The repo's
**Content** tab shows a Trash panel — **Restore** puts a version back exactly as
it was (npm packument entry included), **Purge** hard-deletes it. Trash is swept
automatically after `-trash-retention` (default 7 days; `0` keeps it until
manually purged). Format-native deletes and cleanup runs still hard-delete —
their job is to free space; trash is the human undo path. Prove the loop live
with `scripts/quota-validate.sh` (publish refused at 507, delete → trash →
restore → purge, cleanup still frees space).

---

## Promotion & immutable repositories

Promote a tested artifact from one hosted repository to another — for example
`staging → release` — with `POST /api/v1/repos/{target}/promote`
(`{"sourceRepo","component","version"}`), or the **Promote…** button on a
version in the source repo's **Content** tab. Promotion is a **copy**, not a
reference: the bytes are copied into the target through the target's own format
handler, so its indexes, checksums and packuments regenerate correctly and the
target stays completely self-contained — backup, cleanup, quotas and integrity
all keep working on it with no cross-repo coupling (a later cleanup of the
source can never pull bytes out from under a promoted release). All five formats
promote (maven asset trees, npm tarball + packument, helm charts, CRAN
tarballs, OCI images via a content-addressed manifest walk).

The copy records **provenance** — *promoted from `{sourceRepo}@sha256:{digest}`
by `{actor}` at `{time}`* — stored per target and surfaced on the component
detail pane. This is a historical fact: integrity verify deliberately does
**not** re-check a promoted copy against its source digest, because the copy is
independent by design. A promotion is a hosted write, so the target's storage
quota applies (a promote that won't fit is refused with `507`); it is audited,
fires the `artifact.promoted` webhook, and counts in
`forge_promotions_total{repo}`. Admin on **both** source and target is required.

Mark a hosted repo **Immutable (write-once)** on the repo form (or
`"immutable":true` in the repo API) to make it the natural promotion target: an
existing component+version can never be overwritten — a re-publish or re-promote
returns `409`, soft-delete is refused, and automated cleanup does not run (all of
which would let released bytes change). Publishing a **new** version is still
allowed. Prove the whole loop live with `scripts/promote-validate.sh` (24 checks:
byte-identical copy, provenance, immutable 409s, quota 507, npm packument regen,
clean integrity verify of the target).

---

## Migrating from Nexus

forge imports repositories, content, and permissions from a live Nexus 3
server: `POST /api/v1/migration/plan` produces a persisted dry-run (every
source repo with a create/reuse/skip decision and reason, per-role grant
translation, and what will *not* migrate, stated plainly), `apply` runs the
import as a job on the shared worker, and the **Migration** admin page drives
the whole flow with live per-repo progress. All five formats transfer through
forge's own format handlers (maven asset paths + checksum sidecars, npm
per-version publishes rebuilt from the source packument, helm charts, CRAN
tarballs, docker via a registry-protocol copy that preserves digests); proxy
repos migrate configuration only, and Nexus roles/privileges/content
selectors become grant-carrying forge roles. After each repo the importer
records source-vs-target counts and enqueues a **full integrity verify** —
migration is done when counts match and verify says intact. Re-applying
resumes idempotently. Details: [docs/runbooks/nexus-migration.md](docs/runbooks/nexus-migration.md).

---

## Post-GA roadmap

- **OIDC SSO** — shipped: login against Keycloak/Entra/Okta/ADFS with group→role
  mapping (see [Single sign-on](#single-sign-on-oidc)). **Direct LDAP/AD bind** (no
  IdP broker) and **SAML** remain post-GA; the role-mapping layer is factored so an
  LDAP frontend slots in without rework.
- **CRAN binary trees** (`/bin/`) — per-OS pre-compiled packages; source packages
  work for current use cases.
- **Distributed tracing** — OpenTelemetry integration; Prometheus metrics +
  structured logs cover current operational needs.
- **Chaos suite** — automated pod-kill and S3/PG-blip recovery tests.
- **Cloud Terraform modules** — AWS + GCP modules exist; Azure and nightly
  apply/destroy validation are post-GA.
