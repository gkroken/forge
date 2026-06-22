# WORKPLAN — Horizontal Scaling (N replicas behind a load balancer)

**Status:** S1–S4 **DONE** (2026-06-22). Decision locked and implemented; multi-replica
ready with external storage. Deferred follow-up: audit_log retention (see Phase S2 / Gotchas).
**Date:** 2026-06-22.

**Shipped:** S1 `obs.AuditSink` iface (14578aa) · S2 `PGAuditSink` + migration 003 (3ee8ca1) ·
S3 per-replica UI labels (a62a2b1) · S4 scaling runbook + Helm fs-multi-replica guard (4cc623f).

## Decision

Run Forge as N stateless replicas behind an LB using a **hybrid** observability model.
The three classes of per-process control state get three different homes:

| State | Today (per-pod, in-memory) | Target | Net-new work |
|---|---|---|---|
| Request/cache/latency **metrics** | `obs.GlobalStats`, `obs.RepoStats` (`cachestats.go`), `LatencyTracker`, `ThroughputTracker` | **Prometheus** (already exposed at `/metrics`), aggregate in Grafana | Small — mostly already done |
| **Audit log** | `obs.AuditLog` ring buffer (cap 500) | **Postgres** (durable, coherent, queryable) gated on `POSTGRES_DSN` | The real work |
| **Circuit breakers** | `proxy.go` `breaker` + `globalHealth` `sync.Map` | **Stay per-pod** | None (document only) |

Rationale: metrics are time-series counters that belong in a TSDB, not a relational
store; putting hit/miss in Postgres on every proxy request is an anti-pattern. The
audit log is the one piece that genuinely needs shared durable storage — for both a
coherent Activity tab across pods *and* compliance (records must survive restarts).
Per-pod circuit breakers are *correct*: each replica independently protects its own
outbound calls; a shared breaker (Redis) adds a dependency and a new failure mode for
no clear win at this stage.

The data plane is already replica-ready: `meta.Store`→Postgres (`meta.PG`),
`blob.Store`→S3, queue→Postgres (`queue.NewPG`, auto when `POSTGRES_DSN` is set,
`cmd/forge/main.go:195`). This plan only closes the **control-plane** gap.

## Current-state findings (grounded)

### Metrics — ~90% already in Prometheus
`internal/obs/metrics.go` already registers and exposes:
- `forge_http_requests_total{method,route,status}`, `forge_http_request_duration_seconds{method,route}`
- `forge_proxy_cache_hits_total{repo}`, `forge_proxy_cache_misses_total{repo}`
- `forge_artifact_downloads_total{repo}`, `forge_queue_jobs_total{type,result}`
- Go runtime + process collectors

`GlobalStats` (`globalstats.go`) and `RepoStats` (`cachestats.go`) are a **parallel
in-memory representation** that powers Forge's *own* Dashboard / Observability /
cache-stats panels. They reset on restart and only ever reflect the serving pod. So
"metrics → Prometheus" is **not** "add Prometheus" — it's deciding what the built-in
UI shows under N pods.

### Audit — single ring buffer, swap-friendly call sites
- Constructed once: `obs.NewAuditLog(500)` (`cmd/forge/main.go:208`), injected via
  `Server.WithAuditLog` (`server.go:153`).
- Written in exactly one place: `server.go:529` — and only for **writes / auth
  failures**, not every GET. Write volume is low (good fit for PG).
- Read via `.Recent(n)` in 5 places: `ui_dashboard.go` (×2), `admin.go:795`,
  `ui_admin.go:323`. All consume `[]obs.AuditEntry` newest-first.

The `Append` / `Recent(n)` surface is small and uniform → clean interface seam.

## Plan (incremental, each phase independently shippable)

### Phase S1 — Audit behind an interface (foundation, no behaviour change)
1. Define `obs.AuditSink` interface: `Append(AuditEntry)` + `Recent(n int) []AuditEntry`.
2. Make the existing ring buffer (`*AuditLog`) implement it (it already does).
3. Change `Server.AuditLog` field + `WithAuditLog` to take the interface.
   No call-site changes (signatures already match). Tests stay green.

### Phase S2 — Postgres audit sink (the shared-storage win)
1. `obs.PGAuditSink` backed by `pgxpool` (reuse `pgMeta.DB()`), table:
   `audit_log(id bigserial, ts timestamptz, actor text, method text, path text, status int)`
   with an index on `ts desc`. Schema via the existing migration path used by `meta.PG`.
2. `Append` = INSERT (fire-and-forget with error logging — must never block the request
   path; consider a small buffered channel + single writer if INSERT latency matters).
3. `Recent(n)` = `SELECT ... ORDER BY ts DESC LIMIT n`.
4. Retention: scheduled `DELETE WHERE ts < now() - interval` (or monthly partitions);
   pick one in implementation — note it, don't gold-plate.
5. Wire in `main.go`: when `POSTGRES_DSN` set → `PGAuditSink`, else ring buffer.
   Mirrors `queue.NewPG` vs `queue.NewMem` (`main.go:195-200`).

### Phase S3 — Built-in UI under N pods (decide presentation, small)
Default: label the built-in Dashboard/Observability charts as **"this replica"** so
per-pod numbers aren't mistaken for fleet totals; point operators to Grafana for the
aggregate. (Cheapest, honest.) Optional later: have the UI query Prometheus for the
fleet view — larger, deferrable.

### Phase S4 — Docs + IaC
- `docs/runbooks`: scaling guide — `replicas: N`, shared PG/S3, Prometheus scrape of
  `/metrics`, Grafana dashboard, note that circuit breakers + built-in charts are per-pod.
- Confirm K8s manifests/Helm set `replicas` and don't rely on pod-local audit.

## Explicitly out of scope (this spike)
- Shared circuit breaker (Redis/PG) — per-pod is the chosen behaviour.
- Migrating `GlobalStats`/`RepoStats` into Postgres — they stay per-pod; Prometheus is
  the fleet source of truth.
- Editable group-map UI, SAML, direct LDAP bind (separate tracks).

## Testing
- S1: existing `audit_test.go` still green against the interface.
- S2: `PGAuditSink` integration test using the existing `testcontainers/postgres` setup
  (already a dep) — Append then Recent round-trip, ordering, LIMIT, retention delete.
- Keep `go test ./...` + `test.sh` green throughout.
