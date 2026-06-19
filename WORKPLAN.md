# forge — Workplan: Prototype → GA

This plan takes the working prototype (Maven/npm/Helm/CRAN, hosted + proxy, on a
pluggable `format.Handler` spine) to a production artifact repository. Testing is
treated as a first-class workstream, not a phase at the end: **the test harness
is built before the features it guards.**

**Current status (2026-06-16): all major phases are complete. Coverage gate passing
at 76.1%. Scheduled cleanup shipped. Remaining open items consolidated in §9.**

---

## 1. Definition of Done (GA criteria)

GA is reached when **all** of the following hold:

- ✅ All five formats pass the **conformance suite** driven by real clients
  (`mvn`, `gradle`, `npm`, `pnpm`, `yarn`, `helm`, `docker`/`crane`, `R`/`renv`/`pak`)
  in both hosted and proxy modes.
- ✅ AuthN/AuthZ enforced on every route; authz matrix suite green.
- ✅ Runs HA: ≥2 stateless app nodes against shared Postgres + S3, no sticky state.
- ✅ Meets published SLOs (§6) under the load suite.
- ⚠️ Security gates: SAST, DAST, dependency + container scan, path-traversal and
  authz fuzz suites all green. Third-party pen test scope and threat model documented;
  actual external pen test not yet completed.
- ✅ Migrations are forward + rollback tested on a production-sized dataset.
- ✅ Operability: metrics, structured logs, audit log, health/readiness probes,
  documented runbooks.
- ✅ CI is green on the full client/OS matrix; coverage gates met (§5.10).
- ✅ **Kubernetes-native:** ships a maintained Helm chart; runs HA on K8s with
  HPA, probes, PodDisruptionBudget, and graceful shutdown (adoption req. A).
- ✅ **Infrastructure as Code:** every deployable — image, chart, and cloud
  dependencies (Postgres, object store, IAM, ingress) — is declarative,
  versioned, and GitOps-deployable with no manual steps (adoption req. B).
- ✅ **Easy to set up:** one command to a working eval instance; time-to-first-
  successful-publish < 10 minutes from a clean machine (adoption req. C).

---

## 1a. Adoption requirements — DevOps (must-have)

For DevOps teams to adopt forge, three requirements are **non-negotiable** and
are tracked as first-class acceptance criteria, not nice-to-haves. They are
verified by the deployment test suite (§5.12) and gated in CI.

### A. Hostable in Kubernetes ✅
forge must be a well-behaved K8s citizen:

- ✅ **Stateless app tier** — all state in Postgres + object store; pods are
  disposable and horizontally scalable. No blob data on PVCs.
- ✅ **Maintained Helm chart** (`deploy/helm/forge`) as the primary install path,
  with sane defaults and a documented `values.yaml`.
- ✅ Liveness/readiness/startup probes, `HorizontalPodAutoscaler`,
  `PodDisruptionBudget`, resource requests/limits, graceful shutdown
  (drain in-flight requests on SIGTERM), and configurable `Ingress`.
- ✅ Config via env/`ConfigMap`, secrets via `Secret`; nothing baked into the image.
- ✅ Multi-arch image (amd64/arm64), runs as non-root, read-only root FS.
- ✅ Dogfooding: forge hosts its own Helm chart and OCI images.

### B. Coded as Infrastructure as Code ✅
Everything needed to run forge is declarative and version-controlled:

- ✅ **Helm chart** for the app (templated, linted, versioned, published in CI).
- ✅ **`forge-stack` Helm chart** bundles Postgres + MinIO as sub-charts.
- ✅ **GitOps-ready**: Argo CD `Application` and Flux `HelmRelease` examples in
  `deploy/gitops/`.
- ✅ Image build is reproducible and pinned; releases are signed with SBOM.
- ✅ **Terraform modules** for cloud-managed Postgres/S3 (AWS, GCP, Azure) shipped
  in `deploy/terraform/` — originally scoped post-GA, landed early.

### C. Easy to set up ✅
- ✅ **Eval / local:** `docker compose up` — forge running with FS + FS meta, no
  external deps.
- ✅ **Production:** `helm install forge-stack` with Postgres + MinIO. Quickstart
  gate in CI enforces < 10 minutes from clean machine for both paths.

---

## 2. Guiding principles

1. **Interfaces are the contract.** `blob.Store`, `meta.Store`, and
   `format.Handler` already exist. Every new backend must pass the *same*
   contract test suite. We never special-case a backend above its interface.
2. **Protocol fidelity is the product.** The hardest, highest-value testing is
   "does the real client work?" We invest most heavily there (§5.4).
3. **Test-first on infrastructure.** Storage, auth, and cache get their test
   harness written before/with the implementation.
4. **No feature without tests at the right level** (see test pyramid §5.1).
5. **Golden files for generated artifacts** (maven-metadata.xml, index.yaml,
   PACKAGES, packuments) so format output changes are always reviewed.
6. **Deployment is a product surface.** The Helm chart and Terraform modules are
   versioned, tested, and reviewed like application code — not hand-written at
   release time. The eval on-ramp stays one command throughout.

---

## 3. Workstreams & phases

### Phase 0 — Test & delivery foundation ✅
- ✅ CI pipeline (lint, vet, unit, race detector, coverage upload).
- ✅ Integration harness using ephemeral Postgres + MinIO (testcontainers).
- ✅ **Conformance harness**: real package clients in containers for all five
  formats (`mvn`, `gradle`, `npm`, `pnpm`, `yarn`, `helm`, `crane`, `R`,
  `renv`, `pak`).
- ✅ Golden-file framework + `-update` flag convention.
- ✅ Load-test rig (k6) wired into nightly CI.
- ✅ Coverage gates: overall ≥75%, core packages ≥85%; ratchet enforced. Currently at 76.1%.
- ✅ Multi-arch container image (distroless, non-root) built + published in CI.
- ✅ `docker compose up` eval stack (zero external deps).

### Phase K — Packaging, Kubernetes & IaC ✅
- ✅ Container image: multi-arch, distroless, non-root, read-only root FS,
  SIGTERM graceful drain, `/healthz` + `/readyz`.
- ✅ Helm chart `deploy/helm/forge`: Deployment, Service, Ingress, HPA, PDB,
  ConfigMap/Secret, probes, resource limits, configurable storage backend.
  `helm lint` + schema-validated `values.yaml`.
- ✅ GitOps assets: Argo CD `Application` and Flux `HelmRelease` in `deploy/gitops/`.
- ✅ Quickstart docs + timed setup gate (< 10 min) in CI for both eval and
  production paths.
- ✅ Dogfooding: CI publishes forge's own Helm chart + OCI image to forge.
- ✅ SBOM generation + cosign keyless image signing.
- ✅ Terraform modules for cloud-managed Postgres/S3 in `deploy/terraform/`.

### Phase 1 — Production storage ✅
- ✅ `meta.Store` on Postgres (`internal/meta/pgstore.go`): schema, migrations
  (`internal/meta/migrate/`), connection pooling, transaction boundaries.
- ✅ `blob.Store` on S3/MinIO (`internal/blob/s3store.go`): multipart upload,
  range GET, server-side checksums.
- ✅ Store contract suite passes against FS, Postgres, and S3 backends.
- ❌ Content-addressable dedup option for blobs — not implemented.

### Phase 2 — AuthN / AuthZ ✅
- ✅ User/token model; per-repo role-based permissions (read/write/admin).
- ✅ API tokens; anonymous-read policy per repo.
- ✅ Authz enforced as middleware before handler dispatch; authz matrix suite green.
- ✅ OIDC/SSO login via token-bridge approach — shipped ahead of original post-GA
  scope.

### Phase 3 — Format completeness ✅
- ✅ **Maven:** timestamped SNAPSHOT metadata (`generateSnapshotMetadata`,
  `snapArtifact` records); Gradle `.module` content-type handling; parent-POM
  prefetch in proxy (`prefetchParentPOM`).
- ✅ **npm:** dist-tags (full CRUD at `/-/package/{pkg}/dist-tags`), deprecate,
  unpublish (whole package + single tarball), `npm audit` bridge (returns clean
  report), login/whoami/ping, scoped packages (`@scope/name`).
- ✅ **Helm:** OCI push/pull (`oci://`) via the OCI registry handler; classic
  `index.yaml` hosted + proxy with Bitnami/Helm CLI indent variants.
- ✅ **CRAN:** `PACKAGES` + `PACKAGES.gz` + `PACKAGES.rds` generation;
  per-OS binary trees (`/bin/windows/contrib/`, `/bin/macosx/`) for `.zip` and
  `.tgz`; proxy + group support for binary trees.
- ✅ **Group repositories:** merge `maven-metadata.xml` / `index.yaml` /
  PACKAGES / packuments across ordered members for all five formats.
- ✅ **Docker/OCI registry handler** (Distribution Spec): push/pull for
  container images and Helm OCI charts.

### Phase 4 — Proxy & cache correctness ✅
- ✅ TTL-based freshness; ETag/Last-Modified revalidation (RFC 7232); 304 handling.
- ✅ Negative caching (upstream 404 suppressed for NegativeTTL).
- ✅ Stale-on-error: cached blob served when upstream is down.
- ✅ Circuit-breaker: fast-fail with configurable open timeout and single-probe
  recovery.
- ⚠️ Upstream auth pass-through and per-repo failover ordering — not verified
  in conformance suite.

### Phase 5 — Scale & reliability ✅
- ✅ Stateless app tier; all state in Postgres/S3.
- ✅ Index regeneration via job queue (`internal/queue`) with idempotent workers
  (`internal/indexer`); Postgres-backed queue in production mode.
- ✅ Concurrency-safe: no lost index updates under parallel publish (load suite).
- ⚠️ 24h soak and chaos suite (node kill, S3/PG blip) — load gates run in CI
  but long-form soak and chaos have not been run against a production deployment.

### Phase 6 — UX & operability ✅
- ✅ Admin API: repo CRUD (`/api/v1/repos`), persistent repo config
  (`repo.Manager`), token management.
- ✅ Web UI: home, repo browse, component detail, search, upload, admin,
  token management, access view, login/logout. Dark mode, htmx, server-side
  sort, format icons, proxy health indicators.
- ✅ Metrics: Prometheus counters for proxy cache hits/misses, queue job
  counters; Grafana dashboard + `ServiceMonitor` in Helm chart.
- ✅ Audit log for security-relevant events.
- ✅ Operational runbooks documented.
- ✅ **Scheduled cleanup** (`internal/cleanup`): Nexus-style retention policies
  (`CleanupPolicy`: keepVersions, keepReleasesOnly, deleteSnapshotsDays,
  deleteOlderThanDays, interval). Background scheduler fires per-repo cleanup
  on a configurable cadence. Interval serialised as human-readable string ("24h").

### Phase 7 — Security & GA hardening ✅
- ✅ SAST: `gosec` in CI on every PR.
- ✅ Dependency scan: `govulncheck` in nightly CI.
- ✅ Container scan: Trivy in CI (no High/Critical gate).
- ✅ DAST: ZAP baseline in nightly CI.
- ✅ Path-traversal fuzz suite + authz matrix extensions.
- ✅ SBOM generation + cosign keyless signing on every release.
- ✅ `SEC-001` (upload size limit) and `SEC-002` (group auth leak) fixed.
- ⚠️ Third-party pen test: scope and threat model documented; external pen test
  not yet commissioned.

---

## 4. Sequencing

All phases are complete or substantially complete. See §9 for remaining open
items.

---

## 5. The test suite

### 5.1 Test pyramid

```
        ▲  fewer, slower, highest-confidence
        │   E2E (real deploy, real clients)            ~dozens
        │   Conformance (real CLIs in containers)      ~hundreds   ← biggest bet
        │   Integration (server + PG + MinIO)          ~hundreds
        │   Contract (every Store/Handler impl)        ~hundreds
        │   Unit (pure logic, table-driven)            ~thousands
        ▼  more, faster, cheapest
```

Generated-metadata correctness → **golden-file unit tests**.
"Does the tool actually work" → **conformance**. Backend interchangeability →
**contract**. Wiring → **integration**.

### 5.2 Unit tests
Pure, fast, table-driven, no I/O. Parsers, path/version logic, URL rewriting,
checksum helpers, metadata/index generators (golden files). Run with `-race`.

### 5.3 Contract tests (interface conformance) ✅
One suite per interface run against every implementation (`blobtest.RunContract`,
`metatest.RunContract`). FS, Postgres, and S3 all pass.

### 5.4 Conformance tests (real clients) ✅

| Format | Clients exercised |
|--------|-------------------|
| Maven  | `mvn` 3.9, `gradle` 8 (deploy, dependency:resolve, SNAPSHOT) |
| npm    | `npm`, `pnpm`, `yarn` (publish, install, scoped, dist-tags) |
| Helm   | `helm` 3.x (repo add, push, install), OCI push/pull via `crane` |
| CRAN   | `R` 4.x `install.packages`, `renv`, `pak`; binary packages |
| OCI    | `docker`/`crane` push + pull |

### 5.5 Proxy & cache tests ✅
TTL, negative caching, 304 revalidation, stale-on-error, and circuit-breaker
all covered in `internal/proxy/proxy_test.go`. Nightly live-smoke job hits real
registries.

### 5.6 Security tests ✅
Authz matrix, token lifecycle, path-traversal fuzzing, SAST, DAST, dependency
+ container scanning all wired into CI.

### 5.7 Integration tests ✅
Full server wired to ephemeral Postgres + MinIO via testcontainers.

### 5.8 Performance, soak & chaos
- ✅ **Load (k6):** metadata GET, artifact download, concurrent publish; SLO gates
  in nightly CI.
- ⚠️ **Soak:** 24h sustained load not yet run against a production deployment.
- ⚠️ **Chaos:** node kill + S3/PG blip scenarios not yet exercised.

### 5.9 Migration tests ✅
Up + down migrations tested in CI against a seeded dataset.

### 5.10 Coverage gates ✅
- Overall: ≥75%. Core packages: ≥85%. Gates ratchet; never decrease.
- Per-package enforcement in CI.

### 5.11 Test data & fixtures ✅
Deterministic packages generated per format. Upstream fixtures are recorded
snapshots refreshed by the nightly live-smoke job. Golden files update via
`-update`.

### 5.12 Deployment & IaC tests ✅
- ✅ `helm lint`, `helm template` schema validation in CI.
- ✅ Ephemeral kind cluster install in CI: `helm install` → conformance smoke →
  HPA/PDB/probe assertions → pod-kill-mid-publish.
- ✅ Timed quickstart gate (< 10 min) for both eval and production paths.
- ✅ Container scan: non-root + read-only root FS, multi-arch manifest.

---

## 6. SLOs validated by the load suite

| Metric | Target | Status |
|--------|--------|--------|
| Cached metadata/packument GET, p99 | < 50 ms | ✅ gated in CI |
| Artifact download throughput (per node) | ≥ 1 Gbps aggregate | ✅ gated in CI |
| Concurrent publishes without index loss | ≥ 50 parallel | ✅ gated in CI |
| Availability (HA, single-node failure) | no failed client ops | ✅ pod-kill test |
| 24h soak | no mem growth / latency drift | ⚠️ not yet run |

---

## 7. CI/CD ✅

- **Per-PR:** lint, vet, `go test -race` (unit/contract/integration), coverage
  gate, affected conformance scenarios, SAST (`gosec`), dependency scan
  (`govulncheck`), `helm lint`, container scan (Trivy).
- **Nightly:** full conformance matrix (all clients), live upstream smoke,
  DAST (ZAP), container scan, kind cluster install + quickstart gate, k6 load.
- **Pre-release:** SBOM + cosign signing, image + Helm chart published to forge
  (dogfooding).
- **Matrix:** Go stable + prior, Linux/macOS, client versions per §5.4.

---

## 8. Top risks & mitigations

| Risk | Status |
|------|--------|
| Protocol drift in real clients/upstreams | ✅ Nightly live smoke + version matrix |
| Index regen races under concurrent publish | ✅ Queue + idempotent workers + integration race tests |
| Docker/OCI handler underestimated | ✅ Shipped; conformance green |
| Conformance suite slow/flaky in CI | ✅ Client images cached; quarantine policy in place |
| Storage backend behavioral mismatch | ✅ Single contract suite; all three backends pass |
| Auth retrofitted late | ✅ Shipped before format expansion |
| "Easy setup" erodes as deps grow | ✅ Timed quickstart gate in CI |
| Chart/Terraform rot vs the app | ✅ Linted, plan-tested, cluster-installed every PR |
| K8s claims untested | ✅ Real kind install + pod-kill-mid-publish in nightly CI |

---

## 9. Remaining open items

All are non-blocking for most deployments. Ordered by impact:

1. **Third-party pen test** — scope and threat model are documented; external
   test not yet commissioned. Required for the GA security gate (§1).

2. **24h soak + chaos** — k6 load gates pass in CI but the long-form soak and
   chaos scenarios (node kill, S3/PG blip mid-request) have not been run against
   a production-scale deployment.

3. **Content-addressable blob dedup** (Phase 1) — not implemented. Blobs are
   stored by path, not by hash. Dedup was optional in the original plan; still
   unbuilt.

4. **Upstream auth pass-through + per-repo failover ordering** (Phase 4) —
   proxy circuit-breaker and stale-on-error are implemented, but upstream
   credential forwarding and ordered failover across multiple upstreams are not
   verified in the conformance suite.

5. **npm conformance gap: dist-tags scenario** — dist-tags are fully implemented
   and unit-tested; the conformance suite does not yet exercise `npm publish
   --tag` end-to-end against a real `npm` CLI in a container.

---

## 10. UI Redesign — Foundry direction

Design direction confirmed 2026-06-19: **Foundry (Direction A)** — light
enterprise, IBM Plex Sans/Mono, steel-blue accent `#3a6ea5`.

Reference mockups: `new_design_docs/forge-app-ui-mockup/project/`

- `Forge Admin UI.dc.html` — Repositories list, Browse (3-panel), Repository config
- `Forge Admin - Foundry (remaining tabs).dc.html` — Dashboard, Tokens & Access, Cleanup, Observability

The current admin shell (sidebar, shell layout) already matches the Foundry
structure. What needs to change is detailed below.

### 10.1 Dependency decision required ⚠️

The mockup loads IBM Plex Sans/Mono and Material Symbols Outlined from
Google Fonts CDN. To stay self-contained (eval offline, air-gapped):

- **Preferred**: bundle woff2 subsets as `//go:embed` static assets
- **Alternative**: load from CDN (simpler but breaks offline)

Decide before starting F1.

### 10.2 Foundry color tokens

| CSS var | Hex | Role |
|---|---|---|
| `--bg` | `#fbfcfd` | Content background |
| `--bg-sidebar` | `#f7f9fb` | Sidebar |
| `--bg-card` | `#ffffff` | Cards / panels |
| `--bg-header` | `#f7f9fb` | Table header rows |
| `--border` | `#e7ebf0` | Card / table borders |
| `--border-sidebar` | `#e6eaef` | Sidebar border |
| `--text` | `#1d2733` | Primary text |
| `--text-muted` | `#8a94a3` | Labels / secondary |
| `--text-sub` | `#5b6675` | Tertiary text |
| `--accent` | `#3a6ea5` | Buttons, active nav, links |
| `--accent-light` | `#eaf1f8` | Active nav bg, selected row |
| `--accent-text` | `#1d3f63` | Active nav text |
| `--ok` | `#2e8b6f` | Success / health ok |
| `--warn` | `#c08a2d` | Warning |
| `--err` | `#c0503f` | Error / delete |

### 10.3 What changes vs current UI

**Frontend (templates, CSS):**
- Font stack → IBM Plex Sans 400/500/600/700 + IBM Plex Mono 400/500/600
- Add Material Symbols Outlined icon font
- Topbar: add search bar (300 px cosmetic) + notification/help icon buttons
- Sidebar: add user avatar (initials + role) footer
- KPI cards: add `<span class="ms">` icon + trend row (arrow + Δ)
- Panel section titles → `ALL CAPS` label style (`12px 600 #8a94a3`)
- Recent activity rows: Material Symbol icons per event type (upload / cached / key / cleaning_services)
- Audit log table: actor initials avatar, semantic Action verb (PUT/DELETE/token.create/repo.update), Target column, OK/DENY badge
- Cleanup page: Reclaimable / Freed-30d / Next-run KPIs + Scheduled tasks section
- Tokens page: tab strip (Tokens / Users / Roles), updated table (owner, scoped role badges, last-used, status dot), roles summary cards
- Repositories list: new columns — Format, Type badge, Health, Anon, Artifacts, Size, Updated
- Repository config: tab layout (Settings / Content / Access / Activity) + right rail (storage + proxy cache widgets)
- Browse page: **full rebuild** — 3-panel (repo tree · version list · asset detail)

**Backend (Go):**
- Blob size walker: periodic goroutine computing total + per-format GB; cached result served to dashboard/cleanup
- `obs.Metrics`: P50/P95 latency histograms, rolling 1-min throughput counter
- Token model: add `Owner string` + `LastUsed time.Time` (updated on `Verify`)
- Cleanup: reclaimable estimate (dry-run all enabled policies), freed-30d aggregate from history, last-freed per policy, scheduled task registry (name → cron + last-run)
- Per-version publish timestamps: `VersionInfo.PublishedAt` populated for all five formats (npm packument `time` map, Helm `UploadedAt`, Maven SNAPSHOT timestamp, CRAN DESCRIPTION date)
- Per-version file size: stat blob store key at browse time
- Artifact count per repo: walk blob keys, cache alongside size
- Download counter: increment in middleware on 200 GET, per-repo counter
- Health status per repo: last proxy check result / circuit-breaker state
- Browse tree API: hierarchical path listing for 3-panel left pane
- Users list/CRUD (feeds Users tab)
- Roles CRUD (feeds Roles tab)

### 10.4 Phases

#### F0 — Dependency gate (prerequisite, ~½ day)
- Decide: bundle fonts vs CDN
- If bundle: download IBM Plex Sans woff2 subset + IBM Plex Mono woff2 + Material Symbols subset; place under `internal/server/static/fonts/`
- Update `style.css` with `@font-face` declarations
- No template changes yet

#### F1 — CSS + shell theming (frontend only, ~1 day)
- Update all CSS custom properties to Foundry palette (update dark-mode vars too)
- Update `.admin-topbar`: add search input (cosmetic, no handler) + icon buttons
- Update `.admin-sidebar`: add user avatar footer (initials from auth token description)
- Update `.kpi-card`: add icon row + trend row HTML slots
- Panel section titles → ALL CAPS label pattern
- No backend changes needed

#### F2 — Admin pages reskin (~2–3 days, some backend)

**Dashboard** (needs BE-A for storage sizes + latency):
- KPI: icon + trend values (hardcode trend direction until BE-A)
- Service health: add latency stat column (p50 from BE-A)
- Storage by format: show GB instead of repo count (from BE-A)
- Recent activity: Material icon per event type instead of dots

**Observability** (needs BE-A for latency/throughput):
- KPI → P50/P95/Throughput/Error-rate
- Audit table → initials avatar, Action verb, Target, OK/DENY badge
- Backend: derive semantic action from HTTP method + path; store initials on actor

**Cleanup** (needs BE-A for reclaimable + scheduled tasks):
- KPI → Reclaimable / Freed-30d / Next-run
- Policy table: add Format col, Applied-to col, Last-freed col, Status badge (Enabled/Disabled/Dry-run)
- Add Scheduled tasks section (2-col cards, icon + cron + last-run)

**Tokens & Access** (needs minor backend for owner/last-used):
- Add tab strip (Tokens / Users / Roles) — JS-toggled or htmx fragments
- Token table: Owner, scoped role badges, Repos, Last-used, Status dot, kebab menu
- Roles summary cards (3 cards: Reader / Publisher / Administrator)
- Users tab: placeholder until BE-C
- Backend: add `Owner` + `LastUsed` to token model

#### BE-A — Metrics + cleanup + token data (~1–2 days)
- Blob store walker: background goroutine, result cached in `Server` struct
- `obs.Metrics`: add latency histogram (buckets: 5/25/100/500ms), rolling throughput
- Cleanup: `PolicyManager.Reclaimable() int64`, `PolicyManager.FreedLast30d() int64`, `PolicyManager.NextRun(name string) time.Time`; scheduled task registry in `Scheduler`
- Token `Owner string` + `LastUsed *time.Time`; update on `Verify`

#### F3 — Repository pages (major rebuild, needs BE-B, ~3–4 days)

**Repositories list:**
- New grid columns: Artifacts, Size, Updated, Health, Anon
- Format badge (mono text), Type badge (colored pill: Hosted/Proxy/Group)
- Health dot derived from circuit-breaker / last-proxy state

**Repository config:**
- Tab strip: Settings / Content / Access / Activity (htmx or JS)
- Settings tab: current form fields (unchanged data model)
- Right rail: storage widget (GB + blob count + % quota), proxy cache mini-chart (hit rate), recent activity feed
- Content / Access / Activity: stubs

**Browse page (full rebuild):**
- 3-panel layout: tree pane (288 px) + version list + asset detail (316 px)
- Tree pane: hierarchical folder/package listing with Material icons
- Version list: Version, Size, Modified, Downloads — click selects version
- Asset detail: filename, Download / Copy URL / Delete buttons; metadata rows (Format, Repo, Blob store, Size, Content-type, SHA-256, SHA-1)

#### BE-B — Repository + browse data (~2 days)
- Artifact count per repo (walk + cache in `Server.repoStats` map, refresh on publish/delete)
- Per-version file size: `blob.Store.Stat(key)` → expose via Inspect
- Per-version `PublishedAt` for all formats (npm, Helm, Maven SNAPSHOT, CRAN)
- Download counter: `obs.Metrics` per-repo GET-200 counter
- Health status: export circuit-breaker state per proxy repo
- Browse tree API: `GET /ui/browse/{repo}/tree?prefix=` → JSON; used by the tree pane

#### BE-C — Users + Roles (~3 days, largest scope)
- User model: username, credential (bcrypt or SSO identity), role assignment, created/last-login
- `auth` package: user CRUD (`CreateUser`, `ListUsers`, `DeleteUser`, `SetRole`)
- Roles: pre-defined (Reader/Publisher/Administrator) + custom role support
- Tokens & Access "Users" tab: list + invite + disable
- Tokens & Access "Roles" tab: role cards with member count + permission description
- Admin API: `GET/POST/DELETE /api/v1/users`, `GET/POST/DELETE /api/v1/roles`

---

### 10.5 Repository config page — gap analysis

The design (`Forge Admin UI.dc.html`, "Repository config" section) shows a Settings
form with per-repo proxy tuning fields, and a right rail with storage + cache metrics.
The current `Repository` struct and proxy layer do not support these yet.

#### What the design requires vs what currently exists

**Settings tab — form fields:**

| Design field | Where it maps now | Gap |
|---|---|---|
| Name | `Repository.Name` ✅ | None |
| Blob store (dropdown) | Single global `blob.Store` | Need `BlobStore string` on repo + named store registry |
| Online toggle | No field | Add `Enabled bool`; enforce 503 in routing when false |
| Remote storage URL | `Repository.Upstream` ✅ | None |
| Content max-age | `Repository.ProxyTTL` (partial) | Rename to `ContentMaxAge`; add separate `MetadataMaxAge` |
| Metadata max-age | Not separate from `ProxyTTL` | Add `MetadataMaxAge *time.Duration` |
| Negative cache toggle | Hardcoded in `proxy.Config` | Add `NegativeCache *bool` per repo |
| Auto-block on errors | Hardcoded in `proxy.Config` | Add `AutoBlock *bool` per repo |
| Timeout | Hardcoded per-handler HTTP client | Add `TimeoutSecs *int` per repo |
| Retries | `proxy.Config.MaxRetries` (global) | Add `Retries *int` per repo |
| Circuit breaker status | Exists in proxy package (read-only indicator) | Expose state via `GET /api/v1/repos/{name}/health` (already used by health registry) |

**Right rail — storage widget:**

| Design element | Current state | Gap |
|---|---|---|
| GB cached | Per-repo blob walk (BE-B ✅) | Already wired |
| Blob count | Per-repo count (BE-B ✅) | Already wired |
| % of quota | No per-repo quota concept | Add `QuotaGB *float64` to repo; compute % in API |

**Right rail — proxy cache 24H widget:**

| Design element | Current state | Gap |
|---|---|---|
| 24h hit rate % | Global Prometheus counters only | Need per-repo hourly ring buffer (24 slots × hit/miss) |
| 24-bar chart | Not exposed via API | New endpoint: `GET /api/v1/repos/{name}/cache-stats` |
| Revalidations count | Not tracked separately | Count 304 responses per repo in proxy middleware |
| Negative cache count | Not tracked per repo | Count per-repo negative cache hits |

**Header action buttons:**

| Design button | Current state | Gap |
|---|---|---|
| Invalidate cache | Not implemented | New endpoint `POST /api/v1/repos/{name}/invalidate` — deletes all cached blobs |
| Rebuild index | Reindex job exists | Wire `POST /api/v1/repos/{name}/reindex` to the indexer queue; add UI button |

#### New phases

#### BE-D — Repository model extension (~1.5 days)

Extend `Repository` struct in `internal/repo/repo.go`. Use pointer types so nil means
"use server default", enabling partial per-repo overrides:

```go
// New fields on Repository:
Enabled       bool             `json:"enabled"`              // false → 503 for all requests
BlobStore     string           `json:"blobStore,omitempty"`  // named store; "" → default
ContentMaxAge *time.Duration   // replaces ProxyTTL; nil → DefaultTTL (24h)
MetadataMaxAge *time.Duration  // index/packument TTL; nil → DefaultTTL
NegativeCache  *bool           // override per-repo; nil → global default (true)
AutoBlock      *bool           // circuit-breaker auto-block; nil → global default (true)
TimeoutSecs    *int            // HTTP client timeout; nil → 30s
Retries        *int            // retry count; nil → DefaultMaxRetries (2)
QuotaGB        *float64        // storage quota; nil → unlimited
```

Backward compatibility: `ProxyTTL` stays in JSON as a legacy field; `UnmarshalJSON` copies
it into `ContentMaxAge` if the new field is absent. All new Duration fields follow the
same `MarshalJSON`/`UnmarshalJSON` pattern already used for `ProxyTTL` and `CleanupPolicy`.

Enforcement point for `Enabled`: add a check in `server.go` immediately after repo lookup,
before the format handler is called. Return `503 Service Unavailable` with a JSON body
`{"error":"repository offline"}`.

#### BE-E — Proxy per-repo config wiring (~0.5 days, depends on BE-D)

Add `proxy.ConfigForRepo(r repo.Repository) proxy.Config` in `internal/proxy/proxy.go`.
It merges per-repo fields onto global defaults:

```go
func ConfigForRepo(r repo.Repository) Config {
    c := Config{
        TTL:         orDefault(r.ContentMaxAge, DefaultTTL),
        NegativeTTL: orDefault(r.MetadataMaxAge, DefaultNegativeTTL),
        MaxRetries:  orDefaultInt(r.Retries, DefaultMaxRetries),
        // Timeout: construct http.Client with r.TimeoutSecs or 30s
    }
    if r.NegativeCache != nil && !*r.NegativeCache { c.NegativeTTL = 0 }
    return c
}
```

Each format handler's proxy path calls `proxy.ConfigForRepo(ctx.Repo)` instead of
constructing a hardcoded `proxy.Config{}`. No changes to the proxy algorithm itself.

#### BE-F — Cache hit-rate metrics (~1 day)

Per-repo hourly hit/miss tracking:

- Add `type cacheStats struct` to `internal/server/` or `internal/obs/`:
  `[24]struct{ hits, misses, revalidations, negatives atomic.Uint64 }`, indexed by
  `time.Now().Hour() % 24`.
- Reset a slot when the current hour advances past it (compare stored hour vs now).
- Proxy middleware (in `proxy.Fetch`) increments the appropriate counter after each
  upstream decision: cache hit, cache miss, 304 revalidation, negative cache serve.
- Wire per-repo stats into `Server` (map keyed by repo name) — same pattern as
  `repoStats` from BE-B.
- New endpoint: `GET /api/v1/repos/{name}/cache-stats` returns:
  ```json
  {
    "hit_rate_24h": 0.964,
    "hourly": [{"hour": 0, "hits": 142, "misses": 6}, ...],
    "revalidations": 38,
    "negatives": 4
  }
  ```
- Stats are in-memory only; reset on server restart (acceptable for prototype; note
  in docs as a known limitation).

New endpoint: `POST /api/v1/repos/{name}/invalidate` — walks `blob.Store` for keys under
`{name}/` and deletes any that were fetched from upstream (identified by a `_proxy_`
prefix or metadata flag set at fetch time). Returns `{"deleted": N}`.

#### F4 — Repository config UI rebuild (~2 days, depends on BE-D + BE-E + BE-F)

All changes in `internal/server/templates/repo_config.html` and `repo_config.js`
(new external JS file for the right rail fetches, following the same CSP pattern as
`browse.js`).

**Settings form additions:**
- Blob store: `<select>` rendered from `GET /api/v1/blob-stores`; shows "default" until
  multi-store registry exists (BE-D ships it as a static single-entry list).
- Online toggle: `<label class="toggle">` wrapping a hidden checkbox; posts `enabled=true/false`.
- Proxy section (shown only for `kind = "proxy"`):
  - Content max-age: number input (minutes), converts to/from `time.Duration`
  - Metadata max-age: number input (minutes)
  - Negative cache: toggle switch
  - Auto-block on errors: toggle switch
- HTTP & resilience section (proxy only):
  - Timeout: number input (seconds)
  - Retries: number input
  - Circuit breaker: read-only status chip (`Closed` / `Open` / `Half-open`) fetched
    from `GET /api/v1/repos/{name}/health` on page load.

**Right rail wiring:**
- Storage widget: already rendering blob count + size from BE-B; add quota % row if
  `QuotaGB` is set on the repo.
- 24H cache chart: on DOMContentLoaded fetch `GET /api/v1/repos/{name}/cache-stats`;
  render 24 bars as inline SVG `<rect>` elements (no external charting library). Show
  hit-rate % as headline number. Show revalidations + negatives as secondary rows.
- Recent activity: fetch `GET /api/v1/repos/{name}/activity?limit=10` from the audit
  log filtered by `target = {name}`; render as a timestamped list (same as the
  Dashboard recent-activity feed).

**Action buttons (header):**
- "Invalidate cache": opens confirm modal → `POST /api/v1/repos/{name}/invalidate`;
  shows deleted count in a toast.
- "Rebuild index": confirm modal → `POST /api/v1/repos/{name}/reindex`; shows queued
  confirmation.

**Dependencies and sequencing:**

```
BE-D (model extension)
  └── BE-E (proxy wiring)      ← can parallel with BE-F
  └── BE-F (cache metrics)     ← can parallel with BE-E
        └── F4 (UI)            ← needs all three
```

BE-D and BE-E are low-risk backend changes (additive to the model, backward-compatible
JSON). BE-F is slightly more involved (new in-memory ring buffer + endpoint). F4 is the
UI layer that exposes everything. Build in that order; F4 last.
