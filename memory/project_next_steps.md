---
name: project-next-steps
description: Prioritized roadmap after the Foundry UI design sweep — SSO/auth expansion, proxy singleflight, scaling design spike. Grounded in code as of 2026-06-22.
metadata:
  type: project
---

Backlog discussed 2026-06-22 after completing the Foundry design sweep
([[project-design-sweep]]) and the admin-auth cookie fix ([[project-admin-auth-gotcha]]).
**Status 2026-06-22:** #1 OIDC SSO DONE & live-validated. #2 proxy singleflight DONE
(commit b624600). #3 scaling spike: **decision LOCKED = hybrid** (see WORKPLAN-SCALING.md,
commit 84d6b80); implementation NOT started. Sequencing rationale and grounded findings:

**Recommended order:**

**1. OIDC SSO — DONE 2026-06-22 (commits 23c7946, 52c2c1a, e381e7a, 063434c).**
Productized: `-oidc-*` flags (each defaulting to its `OIDC_*` env), group→role
mapping, SSO user provisioning, read-only config panel on the Access page, README +
Keycloak quick-start. Design was deliberately **LDAP-ready** (user asked to
accommodate AD/Keycloak/SSO): the role mapping + session minting are transport-
neutral, so a future direct LDAP/AD bind reuses them with no rework. The seam:
- `auth.GroupRoleMapper` (`internal/auth/rolemap.go`) — group→base role, highest-wins,
  case-insensitive. Knows nothing about OIDC.
- `(*Server).establishSSOSession(...)` (`internal/server/oidc.go`) — takes
  `(source, subject, email, groups, fallback, ttl)`, resolves role, upserts the User
  (`auth.UserStore.Upsert`, no local password, denies disabled), mints token+cookie.
  `handleOIDCCallback` calls it with source="oidc"; an LDAP handler would call it with
  source="ldap" + its own fallback/ttl.
- OIDC groups come from a configurable claim (`Config.GroupsClaim`, default "groups",
  array or string) extracted in `internal/oidc/oidc.go` `Exchange`.
**Validated live against Keycloak 26** (2026-06-22): full auth-code round-trip driven
with curl. alice (in `forge-admins`) → role=admin, group_matched=true, admin API 200;
bob (no group) → fallback role=read, group_matched=false, admin API 403. Both
provisioned into the Users tab with correct roles. Cosmetic fix during validation:
log the effective groups claim (commit 3276561). Gotcha if re-testing: Keycloak users
need firstName/lastName or they hit a VERIFY_PROFILE required action that blocks the
scripted login.
**Still deferred:** direct LDAP/AD bind (needs a login form + bind frontend; no broker),
SAML, and an editable group-map in the UI (current panel is read-only).

**2. Proxy singleflight — DONE 2026-06-22 (commit b624600).**
Hand-rolled stdlib single-flight (`flightGroup`: map+Mutex+WaitGroup) in
`internal/proxy/proxy.go` — chose stdlib over x/sync/singleflight to honour the
stdlib-only rule (needs narrow: no DoChan/Forget; followers re-read the blob store
rather than sharing the leader's reader). Coalesces concurrent cache misses per blobKey:
leader fetches+stores, followers block then Get the fresh blob. Fast paths (negative
cache, fresh TTL hit) stay OUTSIDE the flight so hits remain fully parallel.
RecordMiss/RecordRevalidation now fire once per herd (leader only). Test
`TestSingleFlight_CoalescesConcurrentMisses` (20 goroutines → 1 upstream call), -race clean.

**3. Scaling — DECISION LOCKED 2026-06-22 = HYBRID (commit 84d6b80, WORKPLAN-SCALING.md).**
Three classes of per-pod control state → three homes:
- **Metrics** (`obs/globalstats.go`, `obs/cachestats.go`) → **Prometheus**. KEY FINDING:
  this is ~90% already built — `metrics.go` exposes cache hits/misses/http/downloads at
  `/metrics`; GlobalStats/RepoStats are just a parallel in-memory copy feeding the
  built-in UI. So little net-new work; built-in charts become "this replica" + Grafana
  for fleet.
- **Audit** (`obs/audit.go` ring buffer) → **Postgres** (gated on `POSTGRES_DSN`, mirror
  `queue.NewPG`/`queue.NewMem`). The ONLY genuine net-new shared-storage work. Append at
  `server.go:529` (writes/auth-failures only, low volume); `.Recent(n)` read in 5 sites.
  Plan: `obs.AuditSink` iface (**S1 DONE**, commit 14578aa) → `PGAuditSink` (**S2 DONE**,
  commit 3ee8ca1: migration 003 audit_log table, async non-blocking writer, auto-on when
  POSTGRES_DSN set, testcontainers PG16 round-trip test) → UI per-replica labels (**S3 DONE**,
  commit a62a2b1: REPLICA hostname chip + "per-replica · see Prometheus/Grafana" notes on
  dashboard/observability charts; `replicaID()` in ui_dashboard.go) → **S4 NEXT = docs/IaC**
  (scaling runbook, confirm K8s manifests set replicas & don't rely on pod-local audit).
  DEFERRED follow-up: audit_log retention (table grows unbounded — scheduled DELETE or
  monthly partitions; noted in WORKPLAN-SCALING.md S2).
  **S4 DONE** (commit 4cc623f): docs/runbooks/scaling.md (external-storage prereq, per-pod
  vs fleet-wide state table, Prometheus/Grafana wiring) + Helm guard that fails render on
  storage.type=fs with replicaCount>1 or autoscaling. **WHOLE SCALING TRACK S1–S4 COMPLETE.**
  Multi-replica ready with external storage (S3+PG). **Audit retention + history DONE**
  (commits 29a2181, a8aa931): automated batched pruner (`-audit-retention`/`AUDIT_RETENTION`,
  default 90d, 0=off); `obs.AuditQuerier` keyset (ts,id) pagination — NO OFFSET — w/ actor/path
  filters; history browser `/ui/admin/audit` (filter form + "Older →" cursor links, linked from
  Observability) + extended `/api/v1/audit` (additive ts/id, X-Next-Cursor-* headers,
  repo_config.js unbroken). NO deferred items remain on the scaling track.
- **Circuit breakers** (`proxy.go` + `globalHealth`) → **stay per-pod** (correct, no work).
Data plane already replica-ready (meta→PG, blob→S3, queue→PG auto on POSTGRES_DSN).
Rejected pure single-pane (PG metrics = anti-pattern) and pure per-pod (loses durable audit).

**4. ROADMAP after scaling (decided 2026-06-22):**
- **Scheduler-HA single-firing — DONE 2026-06-22 (commit 23038ae, WORKPLAN-SCHEDULER-HA.md).**
  Was: `cleanup.Scheduler` ticked per-pod with in-memory `lastRun`, no leader election → every
  pod fired every due scheduled cleanup under N replicas. Fix = `cleanup.Coordinator` seam,
  gated on POSTGRES_DSN (mirrors queue.NewPG/NewMem): `localCoordinator` (eval/FS, default =
  unchanged single-node) vs `PGCoordinator` (`pg_try_advisory_lock` elects one leader per tick
  + shared `lastRun` in `cleanup_schedule_state`, migration 004). Lock alone insufficient —
  shared lastRun load-bearing (follower re-acquiring after release would see its own zero map).
  `scheduler.go` dropped mu/lastRun, gained `WithCoordinator`+exported `Tick`; `RunDue` sig
  unchanged. On-publish `Notify` left per-pod ON PURPOSE (publish hits one pod = already
  single-fire; ticker was the only fan-out gap). Integration test: 2 PGCoordinators race Tick
  on one PG → exactly 1 run. This was ALSO the prereq for the future vuln re-scan scheduler.
- **Webhooks — DONE 2026-06-22 (commits d07e02f W1, 87a8216 W2, WORKPLAN-WEBHOOKS.md).**
  On-publish `artifact.published` events to admin-registered HTTP endpoints, HMAC-SHA256
  signed (`X-Forge-Signature`), **durable via the PG queue** (user-chosen delivery model;
  in-memory in eval). KEY DESIGN: one shared `jobs` table is drained by ONE worker whose
  `dispatch` switch discards unknown types → added `indexer.Worker.Register(typ, handler)`
  seam (a 2nd worker would race+discard); `webhook.deliver` registers, `npm.regen` stays
  built-in; webhook jobs inherit the worker's metrics+task-ring for free. Payload carries
  subID not the secret (honours since-disabled/deleted subs; secret never in queue table).
  Bounded self-managed retry (cap 5, Handle always returns nil so the queue's generic
  immediate-retry can't double-fire); DEFERRED = delayed/exponential backoff (needs a queue
  `visible_after` column). `internal/webhook`: Subscription store (meta ns "webhooks"),
  Sign, Engine (Dispatch+Handle+Deliver). Admin API `GET/POST /api/v1/webhooks`,
  `DELETE /{id}`, `POST /{id}/test`; secret blanked in listings. W2 UI `/ui/admin/webhooks`
  (instrument-panel, Foundry, frontend-design skill) live-verified. On-publish `Notify`
  scoped same as cleanup (the four `/repository/` formats; OCI `/v2/` out). Events: only
  artifact.published for now (extensible via Event.Type). Live-verified end-to-end.
- **NEXT (unstarted):** candidates surveyed — PyPI format (the final extensibility test),
  vuln scanning (deferred, designed in WORKPLAN-VULN.md), artifact signing/SBOM, LDAP bind,
  editable group-map UI, SAML, webhook delayed-backoff, more webhook event types.
- **Vuln scanning = DEFERRED, designed.** Full spike written: `WORKPLAN-VULN.md` — three
  separate phased plans: Plan A OSV (npm+Maven, V0 slice→V1 breadth+obs→V2 warn/block policy),
  Plan B OCI (Trivy/Grype sidecar, NOT in-process), Plan C Helm (config + referenced-image).
  Key design: SOURCE-AGNOSTIC findings store + Security UI + policy engine are the durable
  spine; scanners are pluggable producers. CRAN = unsupported (no credible R source). Policy:
  Off/Warn/Block + threshold + suppressions, per-repo Security tab + global default + named
  policy, ADMIN-set (enforcement is a security boundary; users get visibility + exception
  request flow, never self-downgrade), dry-run preview, gate in handleRepo. Warn is invisible
  to the CLI (forge-side signal only). User will circle back to this.
- **PyPI format = the final architecture-extensibility TEST** — added last to prove a new
  format slots in via the plugin path with no spine changes; bonus: OSV covers PyPI.
- Other candidates surveyed: artifact signing/SBOM (supply-chain siblings), LDAP bind (seam
  pre-built via establishSSOSession), editable group-map UI, OpenAPI spec.

Tracks are independent — SSO doesn't block scaling or vice versa.
