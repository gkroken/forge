---
name: project-next-steps
description: Prioritized roadmap after the Foundry UI design sweep — SSO/auth expansion, proxy singleflight, scaling design spike. Grounded in code as of 2026-06-22.
metadata:
  type: project
---

Backlog discussed 2026-06-22 after completing the Foundry design sweep
([[project-design-sweep]]) and the admin-auth cookie fix ([[project-admin-auth-gotcha]]).
**Status 2026-06-22:** #1 OIDC SSO DONE & live-validated (branch pushed). **NEXT = #2
proxy singleflight**, then #3 scaling spike. Sequencing rationale and grounded findings:

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

**2. Proxy singleflight (quick adjacent win).**
Confirmed **no request coalescing** in `internal/proxy/proxy.go` — N concurrent cache
misses for the same artifact = N upstream fetches (thundering herd). Bites even single-
node. Small, self-contained correctness/efficiency fix.

**3. Scaling — design spike BEFORE code.**
Data plane already scales/replica-ready: `meta.Store`→Postgres, `blob.Store`→S3,
queue→Postgres (auto when `POSTGRES_DSN` set, `cmd/forge/main.go`). The blocker for N
replicas behind an LB is **per-process in-memory control state**:
- `obs/globalstats.go` — request metrics ring buffers (per-pod)
- `obs/cachestats.go` — per-repo cache hit/miss (resets on restart)
- `obs/audit.go` — audit log is an in-memory ring buffer → Activity tab sees only one pod
- `proxy.go` — circuit breakers + `AllHealth()` map keyed by host (per-pod)

**The gating decision:** coherent single-pane view across replicas vs per-pod +
Prometheus aggregation. Single-pane ⇒ move audit + cache-stats to Postgres and share the
circuit breaker (Redis/PG). Per-pod+Prometheus ⇒ much smaller scope. Decide this first.

Tracks are independent — SSO doesn't block scaling or vice versa.
