# forge — Workplan: Prototype → GA

This plan takes the working prototype (Maven/npm/Helm/CRAN, hosted + proxy, on a
pluggable `format.Handler` spine) to a production artifact repository. Testing is
treated as a first-class workstream, not a phase at the end: **the test harness
is built before the features it guards.**

Assumed team: 4–6 engineers. Assumed timeline: ~12 months to GA (range 9–15).

---

## 1. Definition of Done (GA criteria)

GA is reached when **all** of the following hold:

- All four formats pass the **conformance suite** driven by real clients
  (`mvn`, `gradle`, `npm`, `pnpm`, `yarn`, `helm`, `docker`, `R`/`renv`/`pak`)
  in both hosted and proxy modes.
- AuthN/AuthZ enforced on every route; authz matrix suite green.
- Runs HA: ≥2 stateless app nodes against shared Postgres + S3, no sticky state.
- Meets published SLOs (§6) under the load suite, including a 24h soak with no
  leak or latency drift.
- Security gates pass: SAST, DAST, dependency + container scan, path-traversal
  and authz fuzz suites, third-party pen test with no High/Critical open.
- Migrations are forward + rollback tested on a production-sized dataset.
- Operability: metrics, structured logs, audit log, health/readiness probes,
  documented runbooks.
- CI is green on the full client/OS matrix; coverage gates met (§5.10).
- **Kubernetes-native:** ships a maintained Helm chart; runs HA on K8s with
  HPA, probes, PodDisruptionBudget, and graceful shutdown (adoption req. A).
- **Infrastructure as Code:** every deployable — image, chart, and cloud
  dependencies (Postgres, object store, IAM, ingress) — is declarative,
  versioned, and GitOps-deployable with no manual steps (adoption req. B).
- **Easy to set up:** one command to a working eval instance; time-to-first-
  successful-publish < 10 minutes from a clean machine (adoption req. C).

---

## 1a. Adoption requirements — DevOps (must-have)

For DevOps teams to adopt forge, three requirements are **non-negotiable** and
are tracked as first-class acceptance criteria, not nice-to-haves. They are
verified by the deployment test suite (§5.12) and gated in CI.

### A. Hostable in Kubernetes
forge must be a well-behaved K8s citizen:

- **Stateless app tier** — all state in Postgres + object store (the storage
  interfaces already enforce this); pods are disposable and horizontally
  scalable. No blob data on PVCs.
- **Maintained Helm chart** (`deploy/helm/forge`) as the primary install path,
  with sane defaults and a documented `values.yaml`.
- Liveness/readiness/startup probes, `HorizontalPodAutoscaler`,
  `PodDisruptionBudget`, resource requests/limits, graceful shutdown
  (drain in-flight requests on SIGTERM), and configurable `Ingress`.
- Config via env/`ConfigMap`, secrets via `Secret` (or external-secrets);
  nothing baked into the image.
- Multi-arch image (amd64/arm64), runs as non-root, read-only root FS.
- Nice dogfooding signal: forge can host its **own** Helm chart and OCI images.

### B. Coded as Infrastructure as Code
Everything needed to run forge is declarative and version-controlled — a fresh
environment is reproducible from the repo with no click-ops:

- **Helm chart** for the app (templated, linted, versioned, published in CI).
- **Terraform modules** (`deploy/terraform/`) for cloud dependencies: managed
  Postgres, object-storage bucket + lifecycle rules, IAM/service accounts,
  DNS/ingress. Modular per cloud (AWS/GCP/Azure) over a shared interface.
- **GitOps-ready**: chart + values consumable by Argo CD / Flux; example
  `Application`/`Kustomization` provided.
- Image build is reproducible and pinned; releases are signed with SBOM (ties
  into P7). No step in "stand up forge" is a manual console action.

### C. Easy to set up
Two clearly-separated paths, both fast:

- **Eval / local (zero dependencies):** `docker compose up` (or the single
  static binary) → forge running with embedded filesystem + SQLite-class
  metadata, no Postgres/S3 needed. This *is* the prototype's zero-config mode,
  promoted to a supported tier.
- **Production:** `terraform apply` (deps) then `helm install` (app) with a
  documented quickstart. Sensible defaults, generated secrets, schema
  auto-migrate on boot behind a flag.
- **Gate:** a clean machine reaches a successful `npm publish` (or equivalent)
  in **under 10 minutes**, measured by the timed quickstart test (§5.12).

These map directly onto the existing design: `blob.Store`/`meta.Store` give the
eval-vs-production split for free; the single binary gives the easy on-ramp; the
stateless tier makes the K8s story real rather than aspirational.

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

Phases overlap; the table in §4 shows sequencing. Each phase lists deliverables
and **exit criteria** (what must be true + green to call it done).

### Phase 0 — Test & delivery foundation  *(weeks 1–4)*
Build the scaffolding everything else depends on.

- CI pipeline (lint, vet, unit, race detector, coverage upload).
- Integration harness using ephemeral Postgres + MinIO (testcontainers).
- **Conformance harness**: a runner that spins up forge and drives real package
  clients in containers (generalizes the prototype's proven npm-CLI test).
- Golden-file framework + `-update` flag convention.
- Load-test rig (k6) and a seedable test-data generator.
- Coverage gates wired into CI (start lenient, ratchet up).
- **Multi-arch container image** (distroless, non-root) built + published in CI.
- **`docker compose up` eval stack** (zero external deps) — the supported
  easy-setup on-ramp (adoption req. C), live from week 1.

**Exit:** `make test` runs unit + integration + one end-to-end conformance case
(npm proxy) in CI from a clean checkout; coverage reported; `docker compose up`
yields a working instance that accepts a publish.

### Phase K — Packaging, Kubernetes & IaC  *(cross-cutting, weeks 1–48)*
Directly delivers adoption requirements A, B, C (§1a). Starts at week 1 and
matures alongside every other phase rather than landing at the end.

- **Container image** (P0): multi-arch, distroless, non-root, read-only root FS,
  SIGTERM graceful drain, `/healthz` + `/readyz`.
- **Helm chart** `deploy/helm/forge` (matures P0→P6): Deployment, Service,
  Ingress, HPA, PDB, ConfigMap/Secret, probes, resource limits, configurable
  storage backend (eval embedded vs Postgres+S3). `helm lint` + schema-validated
  `values.yaml`.
- **Terraform modules** `deploy/terraform/` (P1, per-cloud): managed Postgres,
  object bucket + lifecycle, IAM/service accounts, DNS/ingress; shared module
  interface across AWS/GCP/Azure.
- **GitOps assets**: example Argo CD `Application` / Flux `Kustomization`.
- **Quickstart docs + timed setup test**: eval path (`docker compose up`) and
  production path (`terraform apply` → `helm install`), both measured against
  the < 10-minute gate.
- **Dogfooding**: CI publishes forge's own Helm chart + OCI image *to forge*.
- Release signing + SBOM land in P7.

**Exit:** `helm install` brings up an HA instance on a clean cluster that passes
the conformance smoke set; `terraform plan` is clean from zero state; the timed
quickstart test is green for both eval and production paths (§5.12).


- `meta.Store` on Postgres (schema, migrations, pooling, tx boundaries).
- `blob.Store` on S3/MinIO (multipart upload, range GET, server-side checksums).
- Content-addressable dedup option for blobs.

**Exit:** the **Store contract suite** (§5.3) passes identically against FS,
Postgres, and S3 backends; migration up/down tested in CI.

### Phase 2 — AuthN / AuthZ  *(weeks 7–13)*
- User/token model; per-repo role-based permissions (read/write/admin).
- OIDC + LDAP/SAML; API tokens; anonymous-read policy per repo.
- Authz enforced as middleware before handler dispatch.

**Exit:** authz matrix suite (§5.6) green; no route reachable without a policy
decision; token lifecycle tests pass.

### Phase 3 — Format completeness  *(weeks 9–28, parallelized per format)*
- **Maven:** timestamped SNAPSHOT metadata; Gradle `.module`; parent-POM
  prefetch in proxy.
- **npm:** dist-tags, deprecate, unpublish, `npm audit` bridge, login flow,
  scoped-package edge cases.
- **Helm:** OCI push/pull (`oci://`) — requires the Docker/OCI registry handler.
- **CRAN:** `PACKAGES.rds` generation; per-OS binary trees under `/bin/`.
- **Group repositories:** merge `maven-metadata.xml` / `index.yaml` / PACKAGES /
  packuments across ordered members.
- **(New surface) Docker/OCI registry handler** (enables Helm OCI; high value).

**Exit:** conformance suite (§5.4) green for every format × {hosted, proxy,
group} × client matrix.

### Phase 4 — Proxy & cache correctness  *(weeks 14–22)*
- TTL, negative caching, ETag/Last-Modified revalidation, stale-on-error.
- Upstream auth, retries, circuit-breaking, per-repo failover ordering.

**Exit:** cache-behavior suite + upstream fault-injection suite (§5.5) green.

### Phase 5 — Scale & reliability  *(weeks 20–34)*
- Stateless app tier; all state in Postgres/S3; index-regeneration via a job
  queue with idempotent workers.
- Concurrency-safe index regeneration (no lost updates under parallel publish).

**Exit:** load SLOs met (§6); 24h soak clean; chaos suite (node kill, S3/PG blip)
recovers without data loss.

### Phase 6 — UX & operability  *(weeks 24–40)*
- Admin API (repo CRUD, users, cleanup policies) + web UI (browse, search,
  upload, admin).
- Search/index service; metrics (Prometheus), tracing, audit log.

**Exit:** API contract tests + UI E2E suite green; dashboards + runbooks exist.

### Phase 7 — Security & GA hardening  *(weeks 36–48)*
- Vulnerability-scanning integration; SBOM; signed releases (Sigstore).
- Pen test; security regression suite; performance re-baseline.

**Exit:** all GA criteria (§1) satisfied.

---

## 4. Sequencing (high level)

```
Q1 | P0 ████  P1 ████████  P2 ████████
Q2 |                 P2 ██  P3 ████████████████  P4 ██████████
Q3 |        P3 (cont) ████  P4 ██  P5 ██████████████  P6 ██████
Q4 |                              P5 ██  P6 ████████████  P7 ████████
PK | ████████████████████████████████████████████████████████████  (cross-cutting)
```

Critical path: **P0 → P1 → P3 (Docker/OCI) → P5 → P7.** Auth (P2) and proxy
(P4) run alongside format work. **Phase K (packaging/K8s/IaC)** runs the full
length: image + compose in P0, Terraform in P1, chart hardening through P6,
signing/SBOM in P7.

---

## 5. The test suite (the backbone)

### 5.1 Test pyramid & where each format concern lives

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
Pure, fast, table-driven, no I/O. Targets: parsers (DESCRIPTION, Chart.yaml,
publish payload), path/version logic, URL rewriting, checksum helpers,
metadata/index generators (with golden files). Run with `-race`.

```go
// maven-metadata.xml is generated; lock its shape with a golden file.
func TestGenerateMetadata_Golden(t *testing.T) {
    c := newTestCtx(t, withBlobs(
        "maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar",
        "maven-hosted/com/acme/lib/1.3.0/lib-1.3.0.jar",
    ))
    got, ok := New().generateMetadata(c)
    require.True(t, ok)
    golden.Assert(t, got, "metadata_two_versions.xml") // -update to refresh
}
```

### 5.3 Contract tests (interface conformance)
One suite per interface, run against **every** implementation. This is what
makes "swap FS→Postgres→S3" safe.

```go
// Run the identical suite against all Store implementations.
func TestBlobStore_Contract(t *testing.T) {
    for name, factory := range map[string]func(t *testing.T) blob.Store{
        "fs":    newFSStore,
        "s3":    newMinioStore,   // testcontainers MinIO
    } {
        t.Run(name, func(t *testing.T) {
            blobtest.RunContract(t, factory(t)) // put/get/stat/list/delete,
                                                // checksums, traversal-reject,
                                                // overwrite, large stream
        })
    }
}
```
The `meta.Store` gets the same treatment (FS vs Postgres).

### 5.4 Conformance tests (real clients) — the crown jewel
For each format we run the **actual** package manager against forge in Docker,
in hosted, proxy, and group modes. This generalizes the prototype test where the
real `npm` CLI installed `is-odd` + its dependency through the proxy.

Client matrix:

| Format | Clients exercised |
|--------|-------------------|
| Maven  | `mvn` 3.6/3.9 (deploy, dependency:resolve), `gradle` 7/8 |
| npm    | `npm`, `pnpm`, `yarn` (publish, install, scoped, dist-tags) |
| Helm   | `helm` 3.x (repo add, push via plugin, install), OCI push/pull |
| CRAN   | `R` 4.x `install.packages`, `renv`, `pak` |
| OCI    | `docker`/`oras` push + pull |

Each scenario asserts: publish succeeds → index/metadata regenerates →
**a fresh client in a clean container installs the published artifact** →
checksums verify. Proxy scenarios assert cache-on-miss then cache-hit-served.

```go
func TestNpm_Conformance_PublishThenInstall(t *testing.T) {
    srv := startForge(t)                      // real binary, PG+MinIO
    runInContainer(t, "node:20", `
        npm publish --registry `+srv.Repo("npm-hosted")+`
        npm install mypkg --registry `+srv.Repo("npm-hosted")+`
        test -f node_modules/mypkg/package.json`)
}
```

### 5.5 Proxy & cache tests
A controllable fake upstream (and recorded fixtures from real registries) lets us
assert: cache-on-read, TTL expiry, 304 revalidation, negative caching,
stale-on-error, upstream-down failover, upstream-auth pass-through. A nightly
**live smoke** hits the real registries to catch upstream protocol drift.

### 5.6 Security tests
Authz matrix (every {role × repo × verb} → expected allow/deny), token lifecycle,
path-traversal fuzzing on every key-bearing route, header/payload fuzzing,
SAST (`gosec`), DAST against a running instance, dependency + container scanning
in CI, and an annual third-party pen test before GA.

### 5.7 Integration tests
Full server wired to ephemeral Postgres + MinIO via testcontainers; assert
cross-component flows (publish → job-queue index regen → read consistency),
concurrent-publish correctness (no lost index updates), and admin-API CRUD.

### 5.8 Performance, soak & chaos
- **Load (k6):** metadata GET, artifact download, concurrent publish; assert
  SLOs (§6) as pass/fail gates, not just dashboards.
- **Soak:** 24h sustained load; fail on memory growth or p99 drift.
- **Chaos:** kill an app node, blip S3/Postgres mid-request; assert recovery and
  zero data loss.

### 5.9 Migration tests
Every schema change ships up + down migrations, tested in CI against a seeded,
production-shaped dataset; rollback must restore a readable state.

### 5.10 Coverage, flakes & quality gates
- Core packages (`blob`, `meta`, `repo`, `format` dispatch, `server`): **≥85%**
  line coverage. Overall **≥75%**. Gates ratchet up; never down.
- Format handlers: judged primarily by conformance, not line coverage.
- **Zero known-flaky tests in `main`.** Flakes are quarantined with an issue and
  fixed within one sprint, not retried.
- PR cannot merge unless: lint+vet clean, unit+contract+integration green,
  affected conformance scenarios green, coverage gate met.

### 5.11 Test data & fixtures
A generator produces deterministic Maven/npm/Helm/CRAN packages (incl. scoped,
SNAPSHOT, multi-version, large-binary). Upstream fixtures are recorded snapshots,
refreshed by the nightly live-smoke job. Golden files live beside their tests and
update via `-update`.

### 5.12 Deployment & IaC tests (adoption reqs A/B/C)
The DevOps acceptance criteria are *tested*, not asserted in prose:

- **Chart correctness:** `helm lint`, `helm template` schema validation, and
  `values.yaml` JSON-schema checks in CI on every change.
- **Real-cluster install:** spin up an ephemeral **kind/k3s** cluster in CI,
  `helm install` forge (with in-cluster Postgres + MinIO), wait for readiness,
  then run the conformance smoke set against the deployed service. Asserts HPA,
  PDB, probes, and graceful drain (delete a pod mid-publish → no failed client
  op).
- **IaC validation:** `terraform fmt -check`, `validate`, and `plan` against
  each cloud module from zero state; `tflint`/`checkov` for policy. A nightly
  apply→destroy in a sandbox account proves the modules actually provision.
- **GitOps dry-run:** Argo CD / Flux render of the example app must reconcile
  cleanly (no drift, no manual steps).
- **Timed quickstart gate (req C):** an automated test starts from a clean
  runner and measures wall-clock to first successful publish for *both* the
  `docker compose` eval path and the `helm install` production path. **Fails the
  build if either exceeds 10 minutes.**
- **Image hygiene:** container scan (no High/Critical), non-root + read-only
  root FS asserted, multi-arch manifest present.

---

## 6. SLOs validated by the load suite

| Metric | Target |
|--------|--------|
| Cached metadata/packument GET, p99 | < 50 ms |
| Artifact download throughput (per node) | ≥ 1 Gbps aggregate |
| Concurrent publishes without index loss | ≥ 50 parallel |
| Availability (HA, single-node failure) | no failed client ops |
| 24h soak | no mem growth / latency drift |

---

## 7. CI/CD

- **Per-PR:** lint, vet, `go test -race` (unit/contract/integration), coverage
  gate, affected conformance scenarios, SAST, dependency scan, `helm lint` +
  `terraform validate`/`plan`, container scan.
- **Nightly:** full conformance matrix (all clients × OS), live upstream smoke,
  DAST, container scan, **kind/k3s `helm install` + timed quickstart gate**,
  Terraform apply→destroy in a sandbox account.
- **Pre-release:** load + soak + chaos, migration up/down on prod-sized data,
  SBOM + signed artifacts, **publish image + Helm chart (dogfooded to forge).**
- **Matrix:** Go (stable, prior), Linux/macOS; client versions per §5.4.

---

## 8. Top risks & mitigations

| Risk | Mitigation |
|------|-----------|
| Protocol drift in real clients/upstreams | Nightly live smoke + version matrix |
| Index regen races under concurrent publish | Job queue + idempotent workers + integration race tests |
| Docker/OCI handler underestimated (biggest new surface) | Start in P3 on the critical path; spike early |
| Conformance suite slow/flaky in CI | Cache client images; shard; quarantine policy (§5.10) |
| Storage backend behavioral mismatch (S3 vs FS) | Single contract suite all backends must pass (§5.3) |
| Auth retrofitted late | Land P2 before format expansion solidifies routes |
| "Easy setup" erodes as deps grow | Timed quickstart gate (§5.12) fails CI past 10 min; eval mode stays dependency-free |
| Chart/Terraform rot vs the app | Treat as product code: linted, plan-tested, and cluster-installed in CI every PR |
| K8s claims untested ("works on my laptop") | Real kind/k3s install + pod-kill-mid-publish assertion in nightly CI |

---

## 9. Immediate next actions (first two weeks)

1. Stand up CI with unit + `-race` + coverage on the current prototype.
2. Write the `meta.Store` / `blob.Store` **contract suites**; make FS pass them.
3. Containerize the conformance runner; port the npm-CLI proof into it.
4. Add the golden-file framework; lock current maven-metadata.xml / index.yaml /
   PACKAGES outputs as the first goldens.
5. Stand up testcontainers Postgres + MinIO so P1 starts test-first.
6. Ship the **container image + `docker compose up` eval stack** and a skeleton
   **Helm chart** (Phase K week 1) so the easy-setup on-ramp exists from day one.
7. Add the **timed quickstart test** (even if generous at first) to lock the
   < 10-minute setup gate into CI before scope grows.
