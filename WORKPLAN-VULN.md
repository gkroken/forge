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
- **Per-format coordinate mapping:** `format.VulnCoordinates` (new optional Handler interface, same idiom as `Inspectable`) — npm/Maven implement it; cran/helm/oci don't and are skipped. Keeps format knowledge in the plugin, not the spine. **DONE** (commit 3).
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
3. **Commit 3 — coordinate seam + npm & Maven mappings. DONE.** Optional Handler iface
   `format.VulnCoordinates`: `OSVCoordinates(component) (ecosystem, name string, ok bool)` (same
   idiom as `Inspectable`; no version param — the caller already holds the version and passes it
   through, so a speculative unused arg is avoided). npm → `("npm", component, true)` (scoped
   names included); Maven → `("Maven", component, true)` — the maven component key is *already*
   `groupId:artifactId`, exactly OSV's Maven name (guarded on the `:` separator). helm/oci/cran
   don't implement → skipped. Compile-time `var _ format.VulnCoordinates` assertions on each.
   Tests: both mappings.
4. **Commit 4 — scan triggers (async only). DONE.** `vuln.scan` job registered via
   `indexer.Worker.Register` (webhook precedent — NO second worker). Orchestration lives in
   `server` (it has the registry + context building, like webhooks): `scanRepo` enumerates the
   repo via `format.Browsable.BrowseRepo` (uniform for npm/Maven, no path-parsing; naturally
   handles npm where the version isn't in the publish path), maps each component via
   `VulnCoordinates`, issues ONE OSV `querybatch` for all component@versions, and writes a Finding
   per version — **including clean results** (empty Advisories = "scanned, no issues" + ScannedAt,
   distinct from "never scanned"). **On-publish enqueue** beside `Scheduler.Notify` (the four
   `/repository/` formats), in a goroutine, **failure-isolated — a missing OSV egress never blocks
   or fails a publish.** Manual admin `POST /api/v1/repos/{repo}/scan` → 202 (501 for
   non-scannable formats, 503 when unconfigured). Handler returns nil on OSV egress failure (logs)
   so the PG queue can't retry-spin/hammer OSV — the A-V1 scheduled re-scan is the safety net.
   Wired in main.go via `WithVuln(store, client)` (before WithQueue + Routes). Tests: scanRepo
   writes findings, skips non-scannable formats, manual endpoint enqueues/202/501/503, enqueue
   no-ops when unconfigured. (Also fixed a pre-existing webhook test-harness cleanup race
   surfaced during this work — separate commit.) **Granularity note:** on-publish re-scans the
   whole repo (one batch call + cached hydration = cheap); per-publish burst redundancy and finer
   granularity are A-V1 concerns (scheduled re-scan + debounce).
5. **Commit 5 — surface: severity badge in browse detail. DONE.** The detail endpoint
   (`/ui/browse/{repo}/detail`) carries a `vuln` block distinguishing four states the UI renders
   differently: unsupported format, supported-but-unscanned, scanned-and-clean (green "no known
   vulnerabilities"), and scanned-with-advisories (worst-severity badge + per-advisory list with
   ID/severity dot/summary/fixed-in/link). Severity tokens themed (light+dark) like the format
   badges; `browse.js` renders `renderSecurity()`; CSP-safe (external JS, no inline). Rebuilt the
   binary after CSS/JS. Tests: `TestBrowseDetail_VulnStates` (all three reachable states).

*Exit (MET):* live-verified end-to-end through the binary — published `lodash@4.17.20` to
npm-hosted, auto + manual scan hit the real OSV API, detail endpoint returned `severity:"high"`
with 5 advisories (summaries, NVD URLs, fixed-in, CVE aliases). The OSV client was also validated
directly against live OSV for npm (lodash) and Maven (log4j-core@2.14.1 → critical Log4Shell),
confirming the `groupId:artifactId` mapping and severity derivation (curated label + CVSS v3.1
scorer; v4.0 vectors stored raw) against real wire data.

## A-V0 STATUS: COMPLETE (2026-06-22)

All five commits landed; `go test ./...` + `go vet` + `bash test.sh` (20/20) green. A pre-existing
webhook test-harness cleanup race surfaced during the work was fixed separately. **Next: A-V1** —
Maven on-publish coverage is already live (the mapping shipped in V0); A-V1 adds the daily re-scan
via the now-HA scheduler, the `/ui/admin/security` keyset page, the dashboard tile, and the
`forge_vulnerable_components` gauge.

### DESIGN — RESOLVED 2026-06-23 (shipped in A-V1): surface vuln data on more than the version-detail page
**Decision (built):** a persisted per-repo `vuln.Rollup` (worst-severity per component, per version,
histogram + count) is computed once at scan time and stored in its own meta ns (`{repo}:vuln-rollup`),
so every surface reads it O(1) via `GetRollup` instead of re-scanning findings per render. Chosen over
recompute-per-render (too costly on large proxy repos) and over a dedicated querier table (premature).
One shared severity-pill component: `.badge-sev` CSS reusing the existing `.sev-*` tokens, a Go `sevBadge`
template helper, and matching JS `sevBadge()`. Surfaces shipped: Browse flat list + maven tree leaves,
version list (+ Content tab), search results, repos-table Security column, dashboard tile. Lookup helpers
return "" for non-vulnerable (no badge) and "unknown" (grey) for present-but-unscored, never a badge when
scanning is off. Original discussion retained below for context.

**Open design question raised 2026-06-22 (decide before/within A-V1).** Today the only surface for
findings is the *version-specific* browse **detail pane** (`renderSecurity` in the right panel) —
i.e. a user only sees a vulnerability after drilling into one exact `component@version`. That's too
narrow: the signal should be visible at the points where someone is *scanning a list*, not only
after they've already picked a single artifact. Reconsider whether detail-pane-only is the right
scope and decide which additional surfaces carry a (worst-)severity badge / count. Candidates:
- **Browse component list** (left/flat pane) — badge next to each package showing its worst
  severity across versions, so a vulnerable package stands out without drilling in.
- **Version list** (center pane) — per-version severity badge, so you see which versions are
  affected vs fixed at a glance.
- **Repo list / `/ui/admin/` repos table** — a per-repo "N vulnerable / M critical" column.
- **Search results** — severity badge on each hit.
- **Repo config → Content tab** — same per-component badge as Browse (manage-in-context).
- **Dashboard tile + Security page + Prometheus gauge** — already planned in A-V1 (the aggregate
  surfaces); this point is specifically about the *per-artifact* signal in list views.
Design considerations: (a) cost — list views enumerate many components, so a per-row
`vuln.Store.Get` per version could be N reads; may want a cheap per-component "worst severity"
rollup cached in the store (or a `List`→map built once per page). (b) the four-state distinction
(unsupported / unscanned / clean / vulnerable) must degrade gracefully in a compact badge (e.g. no
badge when clean/unscanned, grey when unsupported). (c) keep it DRY — one severity-badge component
shared across surfaces. **Recommend deciding the surface set + the rollup-vs-per-version-read
approach as the first task of A-V1**, since the Security page and dashboard tile already need a
repo-wide findings rollup and a shared badge — build that rollup once and reuse it everywhere.

### A-V1 — Breadth + observability — COMPLETE 2026-06-23
1. Maven `OSVCoordinates` (`groupId:artifactId`). **DONE in A-V0** (shipped with the seam).
2. **Daily re-scan** — DONE. Not a second leader/lock: `cleanup.Scheduler.WithTickHook` runs inside the
   existing cleanup leader lock with the shared, persisted lastRun; the server's `VulnRescanTick` enqueues
   a `vuln.scan` per scannable repo once the 24h interval elapses, keyed in a `"__vuln__:"` namespace so it
   never collides with cleanup's per-repo entries. Reuses scheduler-HA wholesale (exactly-once + cross-
   leadership memory, no schema change); keeps `cleanup` free of vuln/queue deps via the plain-typed hook.
3. **Security admin page** `/ui/admin/security` — DONE. All vulnerable findings, filter by repo + min
   severity, keyset-paginated by `(repo, component, version)` (in-memory over per-repo sorted `List`,
   reusing the audit-history page chrome). A dedicated findings-querier table is the noted scale path.
4. Dashboard tile + repos-table Security column + `forge_vulnerable_components{repo,severity}` gauge
   (`obs.Metrics.SetVulnerableComponents`, re-set from the rollup at scan-end + startup sweep) — DONE.
5. Tests: rollup aggregation/degradation, scan persists rollup, surfaces carry severity, no-badge-when-
   unconfigured, re-scan interval/leader gating, Security page list/filter/empty-state — DONE.
   Also fixed a pre-existing cleanup on-publish goroutine/TempDir race (Scheduler now tracks runs in a
   WaitGroup + `Wait()`). `go test ./...` + `go vet` + `bash test.sh` (20/20) green; binary rebuilt.

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
