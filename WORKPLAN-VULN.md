# WORKPLAN — Vulnerability Scanning & Supply-Chain Warnings

**Status:** **STARTED 2026-06-22 — Plan A, A-V0 in progress.** Scope locked to **npm + Maven**
(both OSV-scannable, pure stdlib). Scheduler-HA and webhooks (the prereqs in "Dependencies")
both shipped — the A-V1 daily-re-scan blocker is already cleared. PyPI format is tracked
separately as the architecture-extensibility test (and will be covered for free once it
implements the coordinate seam, since OSV supports the PyPI ecosystem).
**Date:** 2026-06-22.

Surface known-vulnerability advisories for stored/proxied artifacts and let operators
warn on — or block — vulnerable downloads. Designed in three independent plans because the
ecosystems need fundamentally different engines:

- **Plan A — OSV** (npm + Maven): a free, keyless, ecosystem-aware advisory API. The flagship.
- **Plan B — OCI** (Trivy/Grype): real container image scanning; forge holds the bytes.
- **Plan C — Helm**: chart misconfig + referenced-image scanning; a different problem again.

## North-star principle: source-agnostic findings, pluggable scanners

The durable investment is the **findings store + Security UI + policy engine**. Scanners are
**producers** that write into it. This mirrors forge's core "shared spine, formats are plugins"
invariant: the spine (findings/policy/UI) knows nothing about *how* a finding was produced.

```
producers                         spine (durable)                 surfaces
─────────                         ───────────────                 ────────
OSV  (npm, Maven)  ─┐
Trivy (OCI images) ─┼─► vuln.Store  (meta.Store ns)  ─► browse badge
Helm config/image  ─┘   {repo}:vuln  component@ver       Security admin page (keyset)
(future R source)       []Advisory + source + scannedAt   Dashboard tile
                        policy engine (Off/Warn/Block)     Prometheus gauge
                                                           download gate (403/header)
```

A `Finding` carries its `Source` ("osv" | "trivy" | …) so multiple producers coexist per
component without collision. Adding a producer never touches storage, policy, or UI.

## Coverage matrix (honest)

| Format | Producer | Status in this plan | Note |
|--------|----------|---------------------|------|
| npm    | OSV ecosystem `npm`            | Plan A V0 | clean coordinate (name) |
| Maven  | OSV ecosystem `Maven`          | Plan A V1 | coordinate `groupId:artifactId` |
| OCI    | Trivy/Grype (image scan)       | Plan B    | heavy engine; forge holds bytes |
| Helm   | Trivy-config / referenced-image| Plan C    | not a CVE-DB lookup |
| CRAN   | **none credible**              | unsupported | label "not scanned — no R source"; OSV/GHSA lack R; OSS Index R data is too thin to trust |

CRAN gets an explicit "unsupported" state in the UI, never a misleading green "0 issues".

## Existing seams this builds on (grounded)

- **Coordinate enumeration:** `format.Inspectable` (`Inspect`, `ComponentDetail`) — `internal/format/format.go:174`.
- **Per-format coordinate mapping:** a *new* optional Handler interface (same idiom as `Inspectable`) — npm/Maven implement it; cran/helm/oci return `ok=false` and are skipped. Keeps format knowledge in the plugin, not the spine.
- **Async scan jobs:** `queue.Queue` (`internal/queue/queue.go:52`), `queue.NewPG` dedups across replicas via `FOR UPDATE SKIP LOCKED`.
- **Periodic re-scan:** `cleanup.Scheduler` (`internal/cleanup/scheduler.go`) — **but see Dependencies: it is not multi-replica-safe yet.**
- **Findings cache:** `meta.Store` namespace `{repo}:vuln` (FS in eval, Postgres in prod — backend-agnostic, like the proxy cache).
- **Reusable named policy:** `cleanup.PolicyManager` is the precedent for the Security policy object.
- **Surfacing:** browse detail pane; a new Security page reusing the **audit-history keyset pagination** pattern (`internal/server/audit_history.go`); `obs.Metrics` gauge.
- **Enforcement point:** `handleRepo` + `auth.Enforcer` on `/repository/`.
- **Audit + metrics:** durable audit log (`/ui/admin/audit`) records block/exception events; Prometheus gauge fits the per-pod→Grafana model from WORKPLAN-SCALING.

---

# Plan A — OSV (npm + Maven)

OSV.dev: `POST https://api.osv.dev/v1/querybatch` with `{package:{ecosystem,name}, version}`
tuples → advisories with severity (CVSS), affected ranges, aliases (CVE/GHSA), URLs.
Free, no key, pure stdlib `net/http` + JSON (no new dependency).

### A-V0 — Vertical slice (npm + Maven)

Scope locked to **npm + Maven** (Maven is the same OSV producer — only a second coordinate
mapping). Structured as five self-contained commits.

**OSV API best-practice (verified against docs 2026-06-22 — drives the client design):**
`POST /v1/querybatch` returns **only `{id, modified}` per vuln, NOT full details.** Full
advisory data (summary, severity/CVSS, references, fixed versions) requires hydrating each ID
via `GET /v1/vulns/{id}`. The correct pattern (also what `osv-scanner` does):
**batch-query all coordinates → union + dedup the returned IDs → hydrate each *unique* ID once
→ cache hydrated advisories keyed by `id+modified`** (advisories are shared across packages and
updated retroactively; `modified` is the cache-invalidation key). This avoids both the
query-per-package anti-pattern (chatty) and re-hydrating the same CVE N times.
Use the `name`+`ecosystem`+`version` request form (NOT a versioned PURL — sending both `version`
and a versioned purl is a documented 400). HTTP/2 negotiated automatically over TLS by
`net/http` (matters: OSV caps HTTP/1.1 responses at 32 MiB). No new dep.

1. **Commit 1 — model + store (source-agnostic spine).** `internal/vuln`: `Severity` (ordered
   enum), `Advisory{ID, Aliases, Summary, Severity, CVSS, FixedIn, URL}`,
   `Finding{Component, Version, Source, Advisories, ScannedAt}`. Store CVSS vector raw **and** a
   derived ordered severity bucket (so UI/policy compare without re-parsing). `vuln.Store` over
   `meta.Store` ns `{repo}:vuln`, key `component@version`, idempotent re-scannable `Put`. Tests:
   round-trip, severity ordering.
2. **Commit 2 — OSV client (two-step + advisory cache).** querybatch → dedup IDs → hydrate
   `/v1/vulns/{id}` with `id+modified` cache; bounded timeout, retry/back-off (mirror
   `internal/proxy`); **graceful degrade** on egress failure (return cleanly, never panic/block).
   Tests: httptest OSV stub — batch→hydrate, cache hit skips re-fetch, clean result, 5xx retry,
   timeout/egress-fail returns cleanly.
3. **Commit 3 — coordinate seam + npm & Maven mappings.** Optional Handler iface
   `OSVCoordinates(component, version) (eco, name string, ok bool)` (same idiom as `Inspectable`).
   npm → `("npm", component, true)`; Maven → `("Maven", "groupId:artifactId", true)`;
   helm/oci/cran don't implement → skipped. Tests: both mappings.
4. **Commit 4 — scan triggers (async only).** Scan job registered via existing
   `indexer.Worker.Register(typ, handler)` (webhook precedent — NO second worker, which would
   race+discard). Handler: coordinates → OSV → `vuln.Store.Put`. **On-publish enqueue** at the
   same point webhooks emit (the four `/repository/` formats), **failure-isolated — a missing OSV
   egress must never block or fail a publish.** Manual admin `POST /api/v1/repos/{repo}/scan`
   ("Scan now"). Tests: on-publish enqueues, handler writes finding, unknown-coordinate skips.
5. **Commit 5 — surface: severity badge in browse detail.** npm + Maven detail path carries the
   worst-severity finding per version; render a badge in the right (detail) pane. Rebuild the
   binary after JS/CSS. Live-verify: publish a known-vuln npm pkg (e.g. old `lodash`) and a
   known-CVE Maven artifact → badge shows the advisory.

*Exit:* publish a known-vulnerable npm or Maven artifact → badge shows the advisory.

### A-V1 — Breadth + observability
1. Maven `OSVCoordinates` (`groupId:artifactId`).
2. **Daily re-scan** via the Scheduler — advisories are published *retroactively*, so periodic re-scan is mandatory. **Hard-depends on scheduler-HA** (else N replicas re-scan in parallel).
3. **Security admin page** `/ui/admin/security`: all findings, filter by repo/severity, **keyset-paginated** (reuse the audit-history mechanics).
4. Dashboard tile ("N vulnerable components, M critical") + `forge_vulnerable_components{repo,severity}` Prometheus gauge.
5. Tests: Maven mapping, re-scan idempotency, page pagination/filter.

### A-V2 — Policy & enforcement (warn vs block)
The full design from the design discussion:

- **Policy object** (`Off | Warn | Block`, severity **threshold**, **fail-open** for unscanned, **suppressions** = CVE/GHSA IDs ignored *with reason + who/when*, audited).
- **Where it lives:** a **global default** + **per-repo** override on a new **Security tab** in repo config (Settings/Content/Access/Activity/**Security**), optionally referencing a **named** Security policy (cleanup-policy precedent) so "strict"/"lenient" is configured once and applied to many.
- **Who sets it:** **admin-only** in V2.0 (enforcement is a security boundary — consumers must not self-downgrade Block→Warn). Fast-follows: **delegated owner** (repo-scoped manage capability for write-role owners) and a **user exception/acknowledge flow** (request → admin grants a time-boxed override; never self-service). Exceptions are audit events.
- **Enforcement point:** gate in `handleRepo` *after* auth, *before* serving the artifact (jar/tarball only, not metadata/checksums); map path → `(component, version)` via the coordinate seam; **Warn** = serve + `X-Forge-Vulnerabilities` header + audit + metric; **Block** = 403 with advisory link.
- **Dry-run preview** (cleanup-dry-run precedent): "switching to Block would 403 N components / M versions" — shows blast radius before enforcing.
- **Honest caveat:** *Warn is invisible to the CLI.* `npm install` / Maven won't render forge's warning; a warn is a forge-side governance signal (UI/header/audit/metric), not an install-time message. Only Block is visible to the client (as a failed fetch). Document so "warn" isn't mistaken for broken.
- Multi-replica: policy in `meta.Store` (PG) = fleet-coherent; `forge_downloads_blocked_total{repo}` metric.
- Tests: threshold/suppression logic, gate 403/serve, dry-run count, fail-open.

---

# Plan B — OCI image scanning (Trivy / Grype)

Real container scanning: crack image layers, enumerate OS packages (apk/deb/rpm) + bundled
language deps, match against vuln DBs (which include OSV + distro advisories). **forge hosts the
image bytes** (docker-hosted), so this is feasible and high-value — unlike Helm's external refs.

**Engine decision:** Trivy/Grype are large Go modules that download their own vuln DBs →
clashes with stdlib-only. Run as an **external sidecar/binary** (`trivy image` pointed at the
registry), *not* in-process. Findings flow into the **same** `vuln.Store`/UI/policy as Plan A.

- **B-O0 — Spike:** stand up Trivy as a sidecar; scan a forge-hosted image via the registry API; parse JSON → `Finding{Source:"trivy"}`. Validate auth/network path.
- **B-O1 — Integration:** on-push scan job (queue) + manual; write findings; badge on OCI component detail; reuse Security page.
- **B-O2 — Policy:** reuse the Plan A-V2 policy engine and download gate for OCI pulls (`/v2/...`). Same Off/Warn/Block semantics.
- **Open:** DB refresh cadence for Trivy; airgapped DB mirroring; per-layer vs per-image granularity; scan cost on large images (async only, never inline).

---

# Plan C — Helm

Helm charts are **not** a package ecosystem with a CVE DB — wrong tool. Two distinct concerns,
both lower priority:

- **C-1 — Chart misconfiguration:** insecure K8s settings in templates → config scanners
  (Trivy-config / kube-linter / Checkov). Static analysis, not advisory lookup.
- **C-2 — Referenced images:** parse `values.yaml` for image refs → scan via the **Plan B**
  pipeline. Caveat: those images usually live in *external* registries forge doesn't store,
  so coverage is best-effort.

Recommend deferring Helm until A + B are proven; it reuses B's engine for C-2.

---

# Dependencies, sequencing, scope

**Hard dependency:** Plan A-V1's daily re-scan (and any periodic scan in B) requires
**scheduler-HA (single-firing under N replicas)** — currently `cleanup.Scheduler` ticks per-pod
with an in-memory `lastRun` and no leader election (`scheduler.go:53`), so N replicas would
re-scan in parallel. **Do scheduler-HA first.** (Tracked outside this workplan; it is the
immediate next task.)

**Recommended order:** scheduler-HA → A-V0 → A-V1 → A-V2 → B → C.

**Out of scope:** CRAN advisories (no source), in-process scanning engines for OCI (sidecar
only), SBOM export and artifact signing (sibling supply-chain epics — separate workplans),
package-manager-native warning injection (hacky).

**Open questions:** OSV rate/batch limits at scale; airgapped story (OSV offers downloadable
per-ecosystem DB dumps — future mirror mode); whether suppressions are global or per-repo;
severity normalization across OSV CVSS vs Trivy severities.

**Constraints (unchanged):** Go stdlib only (flag any new dep — Trivy is a *sidecar binary*,
not a Go import, to stay within this); keep `go test ./...` green; commit per self-contained
unit; rebuild the binary after CSS/JS changes.
