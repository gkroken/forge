# WORKPLAN — Vulnerability Scanning & Supply-Chain Warnings

**Status:** **Plan A COMPLETE (A-V0 + A-V1 + A-V2) as of 2026-06-23.** Findings, surfacing,
breadth/observability, and the warn/block download policy all shipped for **npm + Maven**.
Next ecosystems: Plan B (OCI/Trivy sidecar), Plan C (Helm). Original scope note follows.
Scope locked to **npm + Maven**
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

### A-V2 — Policy & enforcement (warn vs block) — COMPLETE 2026-06-23
Shipped across self-contained commits on `feature/foundry-remaining-tabs`:
- **Policy spine** (`internal/vuln/policy.go`): `Policy{Mode Off|Warn|Block, Threshold,
  FailOpen, Suppressions}` + a pure `Decision(finding, found)` evaluator (clean/unscored
  findings always serve; suppression filters advisories by id/alias before worst-severity;
  fail-open governs unscanned). `PolicyManager` mirrors `cleanup.PolicyManager`: named
  policies in meta ns `vuln-policies`, a global default in `vuln-policy`, `Resolve(name)` =
  named → default → Off.
- **Path→coordinate seam** (`format.VulnGate`): reverses a download sub-path to the
  `(component, version)` keys `vuln.Store` holds, `ok=false` for non-primary artifacts.
  npm (tarball paths) + Maven (jar/war/aar/ear only, mirroring BrowseRepo's verIdx model);
  helm/cran/oci never gated. (A sibling to `VulnCoordinates`, which can't carry path
  reversal — noted divergence from the original "via the VulnCoordinates seam" wording.)
- **Gate** in `handleRepo` (`vuln_gate.go`): GET/HEAD of a primary artifact, after auth,
  before serve. Block = 403 + advisory link; Warn = serve + `X-Forge-Vulnerabilities`
  header. Each decision: durable audit row (new `AuditEntry.Detail`, migration 006),
  `policy.violation` webhook (new event type), `forge_downloads_blocked_total{repo}` (block
  only). Fails open on every uncertainty; fail-closed-for-unscanned is the explicit
  `Policy.FailOpen` choice.
- **Admin API** (`admin_security.go`): named-policy CRUD + `_default` + repo assignment
  (`/api/v1/repos/{name}/security-policy`), suppressions stamped with who/when on save.
  Dry-run blast radius (`.../security-policy/dry-run`).
- **UI**: `/ui/admin/security-policies` (global default + named policies, Findings|Policies
  sub-nav) and a per-repo **Security tab** (resolved policy, assignment, suppressions view,
  Preview impact, ungateable-format note). `.chip-warn` token added.
- Tests: evaluator (threshold/suppression/fail-open/clean), both VulnGate mappings, gate
  (403/serve/header/fail-open/off/ungateable/not-configured), API CRUD + assignment + dry
  run. `go test ./...` + `go vet` + `bash test.sh` (20/20) green; binary rebuilt.
- **Fast-follows (not in V2.0):** delegated owner (repo-scoped manage capability) and a
  user exception/acknowledge flow (request → admin grants time-boxed override) — both
  audit events. The `policy.violation` event is now live for the webhook spine.

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

## Plan B — Grounded design (2026-06-29)

### Seams already in place — no changes needed
- `vuln.SourceTrivy = "trivy"` (`vuln.go:22`) — constant exists; `Finding{Source:"trivy"}` slots into
  the same store, Security page, badge, and policy engine with zero spine changes.
- `vuln.Store` + `Rollup` + badge — browse detail calls `vuln.Store.Get(repo, image, tag)` and will
  show Trivy findings with no UI change.
- `pol.Decision(f, found)` — source-agnostic; doesn't care if advisories came from OSV or Trivy.
- `indexer.Worker.Register` — same shared worker as webhook delivery and OSV scans.
- OCI `BrowseRepo` (`oci.go:510`) — returns `BrowseEntry{Name:image, Versions:[]string{tags}}` —
  same shape as npm/Maven; a repo-wide Trivy scan can enumerate it identically.
- On-push hook in middleware (`server.go:703`) — `ociManifestRef` block detects manifest PUT and
  emits a webhook event; a parallel `enqueueTrivyScan` goroutine fits here.

### New code required per phase

**B-O0 — Trivy exec wrapper + server wiring + on-push enqueue**

`internal/trivy` package (stdlib only: `os/exec`, `encoding/json`, `os`):
- `Executor` interface: `Run(ctx, env []string, args ...string) ([]byte, error)` — injected so
  tests provide a fake without spawning a real process. `osExecutor` is the real impl.
- `Scanner` struct: `Binary, RegistryAddr, AuthToken string` (Binary defaults to `"trivy"` in PATH,
  RegistryAddr is forge's `/v2/` host e.g. `localhost:8080`, AuthToken passed as `TRIVY_REGISTRY_TOKEN`).
- `ImageRef(repoName, image, tag)` → `"{RegistryAddr}/{repoName}/{image}:{tag}"` — what docker
  clients (and Trivy) use against forge's OCI registry.
- `ScanImage(ctx, ref)` → runs `trivy image --format json --quiet --insecure {ref}` with optional
  `TRIVY_REGISTRY_TOKEN` env; parses `Results[].Vulnerabilities[]` → `[]vuln.Advisory`.
  Deduplicates by `VulnerabilityID` across layers. Trivy severities
  CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN map to our SeverityCritical/High/Moderate/Low/Unknown bucket.
  Stdout is parsed even on non-zero exit (trivy exits 1 with `--exit-code 1`; default is 0).

Server additions (`server/trivy_scan.go`):
- `trivyScanJobType = "trivy.scan.oci"` + `trivyScanPayload{Repo, Image, Tag}`.
- `Server.Trivy *trivy.Scanner` field, `WithTrivy` method; registered in `WithQueue` when non-nil.
- `scanOCIImage(ctx, repoName, image, tag)` — builds ref via `Trivy.ImageRef`, calls `ScanImage`,
  writes `Finding{Source:"trivy"}`, then rebuilds rollup from `Vuln.List` (Trivy scans one image/tag
  at a time unlike OSV's whole-repo batch) → `PutRollup` + `SetVulnerableComponents`.
- `enqueueTrivyScan(repoName, image, tag)` — goroutine enqueue, failure-isolated.
- Flags: `-trivy-binary` (default `"trivy"`), `-trivy-addr` (required to enable), `-trivy-auth-token`.

On-push: refactor the `ociManifestRef` block in middleware to not be gated on `s.Webhooks != nil`;
add `enqueueTrivyScan` call for tag refs (skip `sha256:` digest refs — not primary tag publishes).

**B-O1 — Full repo scan + manual + daily**
- `scanOCIRepo(ctx, repoName)` — enumerate via `BrowseRepo`, call `scanOCIImage` per image/tag.
- `handleVulnScan` extended: OCI repos with Trivy configured → enqueue trivy job (currently 501).
- `VulnRescanTick` extended: also enqueue trivy scans for OCI repos when `s.Trivy != nil`.
- Badge and Security page: zero changes — Trivy findings slot in via the existing vuln.Store.

**B-O2 — Policy gate on OCI manifest pulls**
- OCI handler implements `format.VulnGate`: `VulnGateTarget(sub)` parses sub → returns
  `(image, ref, op=="manifests" && !isDigest(ref))`. Tag-based manifest GETs are gated; digest
  refs fail open (safe direction — worst case a vulnerable image bypasses gating by digest; noted
  fast-follow: reverse digest→tag lookup in OCI meta store).
- `handleOCI` gets the same 3-line gate call as `handleRepo` (only on GET/HEAD).

### Open questions resolved
| Question | Decision |
|---|---|
| Per-layer vs per-image? | Per-image (one Finding/tag, advisory list = union of all CVEs across layers). |
| Async only? | Yes — never inline; Trivy can take 10–30s on large images. |
| Auth for Trivy | `-trivy-auth-token` → `TRIVY_REGISTRY_TOKEN` env. Eval (no auth) works without it. |
| Trivy DB refresh | Trivy auto-downloads DB to `~/.cache/trivy`; a `CronJob` in Helm handles `--download-db-only` daily. No forge code. |
| Digest pulls gated? | Not in B-O2; fails open. Fast-follow: reverse digest→tag lookup. |

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

## C-0 — Document the Trivy requirement + validate the real binary (prerequisite — added 2026-06-30)

**Why this is a prerequisite, not an afterthought.** Plan B shipped the Trivy *code* (exec
wrapper, on-push job, policy gate) with fake-executor unit tests, but the real binary path was
never exercised and the binary is not present in any forge artifact:

- The runtime image is `gcr.io/distroless/static-debian12:nonroot` — **no shell, no package
  manager, no trivy.** `internal/trivy` calls `exec.CommandContext(ctx, "trivy", …)`, which
  needs the binary **in forge's own container PATH** (it is an in-container subprocess, *not* a
  k8s sidecar despite the package comment). So on the stock image, setting `-trivy-addr` makes
  every scan fail with "executable file not found".
- The CI `docker` job runs `aquasecurity/trivy-action` to scan *forge's own image* — unrelated
  to forge-the-product scanning artifacts it hosts. No CI job exercises the B-O0 path.

C-1 and C-2 add **two more** trivy-dependent code paths (`trivy config`, `trivy image` against
external refs). All three share this one gap, so close it first.

**Audiences (keep straight):** *consumers* (npm/helm/docker pull) never need trivy. Only an
*operator who opts into scanning* does. Scanning stays **off by default**; `s.Trivy == nil` ⇒
no-op everywhere.

### Decision: operator-supplied, well-documented (Option D) — do NOT package trivy in Helm

Scanning is an opt-in feature most operators won't enable. Building it into the Helm chart (an
init container + DB PVC + refresh CronJob + schema + CI) is permanent chart complexity to serve a
minority — over-engineering for the current stage. Forge instead treats trivy as a documented
external dependency the operator supplies (the normal "bring your own X" pattern), and keeps the
default image tiny and distroless. Revisit packaging (the previously-considered init-container
approach) only if scanning becomes a headline feature with real demand.

### C-0 deliverables (docs + one-time validation — **no Go, no Helm changes**)

1. **Document the requirement clearly** (README / setup.md, by the `-trivy-addr` flag): scanning
   needs the `trivy` binary reachable on forge's PATH; the stock distroless image does not include
   it; how to enable (build a forge image with trivy added, or run forge on a host/image that has
   it). One concrete example snippet, not a full supported integration.
2. **One-time local validation (closes the B-O0 gap):** install trivy locally, run forge, push one
   image to a forge OCI repo, run a real scan, and confirm the live trivy JSON still matches
   `parseOutput` (guards against trivy schema/flag drift). This is the "validate auth/network
   path" exit criterion B-O0 never met. Record the trivy version validated against.

**Exit:** the trivy requirement is documented so an operator can enable scanning without guesswork,
and the real-binary path has been exercised once against live trivy output. Default install is
unchanged (still the tiny distroless image, scanning off).

#### Validation run — 2026-06-30 (deliverable #2 DONE; docs #1 still open)

Live-validated the Plan B real-binary path end-to-end against **trivy v0.72.0**:
`docker push localhost:8080/docker-hosted/alpine:3.12` → on-push hook **and** manual
`POST /api/v1/repos/docker-hosted/scan` both fired → `trivy image --format json` ran → finding
written to `vuln.Store`. Forge's stored advisory (`CVE-2022-37434`, critical, fixedIn `1.2.12-r2`)
matched a direct `trivy` run exactly, confirming `parseOutput` handles v0.72.0's JSON schema.
Rollup + Prometheus gauge populated correctly.

**Bug found & fixed (live validation caught what fake-executor unit tests missed):** the browse
**detail pane** reported OCI images as "not scanned — unsupported" even with a stored Trivy
finding. `vulnInfoFor` (`server/browse.go`) keyed `Supported` solely on `format.VulnCoordinates`
(OSV — npm/Maven only), so OCI never reached the finding lookup. The rollup-driven surfaces (list
badge, dashboard, Security page, gauge) were unaffected, so the two disagreed. Fixed:
`Supported = osvSupported || trivyScannable(format)`. Regression test `TestVulnInfoFor_OCITrivy`
covers OCI-with-Trivy (supported) and OCI-without-Trivy (unsupported). `go test ./...` + `go vet`
+ `bash test.sh` (20/20) green.

Still open in C-0: deliverable **#1** (operator-facing docs for the trivy requirement).

### Sub-track renumber (after C-0)

- **C-1 — Chart misconfiguration** (`trivy config`): `Source:"trivy-config"`; new
  `Scanner.ScanConfigFile` + `Misconfigurations[]` parser (distinct from `Vulnerabilities[]`).
- **C-2 — Referenced images:** `Source:"trivy-helm-image"`; parse `values.yaml` image refs →
  `Scanner.ScanExternalImage` (no `TRIVY_REGISTRY_TOKEN`; pulls from external registries) →
  reuse Plan B parser. Plus Helm `VulnGateTarget` + on-upload/daily/manual scan wiring.

C-0 is the gate; C-1 and C-2 build on it.

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
