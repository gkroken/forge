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
- **Circuit breakers** (`proxy.go` + `globalHealth`) → **stay per-pod** (correct, no work).
Data plane already replica-ready (meta→PG, blob→S3, queue→PG auto on POSTGRES_DSN).
Rejected pure single-pane (PG metrics = anti-pattern) and pure per-pod (loses durable audit).

Tracks are independent — SSO doesn't block scaling or vice versa.
