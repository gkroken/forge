# WORKPLAN — Scheduler HA (single-firing under N replicas)

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

## Problem

`cleanup.Scheduler.loop` (`internal/cleanup/scheduler.go`) ticks every minute and
calls `RunDue` against a **per-pod in-memory `lastRun` map**. There is no leader
election or advisory lock. Under N replicas every pod is its own leader and fires
every due scheduled cleanup independently. (The PG `jobs` queue dedups index-regen
via `FOR UPDATE SKIP LOCKED`, but scheduled cleanup never touches the queue.)

This is also a prerequisite for the future vuln re-scan scheduler.

## Decision (LOCKED)

Postgres **advisory lock** gates the tick; only the lock-holder fires. Shared
`lastRun` is **persisted in Postgres** so leadership can move between pods without
re-firing a due job (the lock alone is insufficient — a follower acquiring the lock
after the leader releases would see its own zero `lastRun` and re-fire).

Both pieces gated on `POSTGRES_DSN`, mirroring `queue.NewPG`/`queue.NewMem`:
- **eval/FS mode** keeps today's single-node behavior (in-memory `lastRun`, always leader).
- **PG mode** uses `pg_try_advisory_lock` + a dedicated state table.

Shared-state store = **dedicated table via migration 004** (not a meta.Store
namespace). Rationale: keep the lock and the state it guards in the same backend
(couplable in one connection/tx), proper `TIMESTAMPTZ` typing, and consistency with
the `PGAuditSink` + migration-003 precedent. Avoids the anti-pattern of straddling
two storage abstractions for one atomic invariant.

## Design

New `cleanup.Coordinator` interface:
```
RunExclusive(ctx, fn func(lastRun map[string]time.Time)) error  // holds leader lock, passes+persists shared lastRun; no-op if not leader
Snapshot(ctx) (map[string]time.Time, error)                     // backs LastRuns() for the UI
```
- `localCoordinator` (default): always leader, in-memory map + RWMutex = today's behavior.
- `PGCoordinator` (when POSTGRES_DSN set): `pg_try_advisory_lock(key)` on a pinned
  `*sql.Conn` (non-blocking; not acquired ⇒ no-op). Loads/persists `lastRun` from
  `cleanup_schedule_state(repo PK, last_run TIMESTAMPTZ)`.

Lock held for the whole tick (decide → fire → persist). Cleanup runs are short;
followers no-op and retry next minute. Crash mid-run is self-healing (`RunForRepo`
is idempotent).

Out of scope: the on-publish `Notify` path stays per-pod — a publish is
load-balanced to exactly one pod, so it is already single-fire. The minute ticker
is the only fan-out gap.

## Tasks

- [x] 1. `internal/meta/migrate/sql/004_up.sql` + `004_down.sql` — `cleanup_schedule_state` table.
- [x] 2. `internal/cleanup/coordinator.go` — `Coordinator` interface + `localCoordinator`.
- [x] 3. `internal/cleanup/coordinator_pg.go` — `PGCoordinator` (advisory lock + upsert/select).
- [x] 4. `scheduler.go` refactor — dropped `mu`/`lastRun` fields; `NewScheduler` defaults
       to `localCoordinator`; added `WithCoordinator(c)`; `loop` → exported `Tick(ctx, now)`
       wrapping `coord.RunExclusive`; `RunDue` signature unchanged; `LastRuns()` delegates
       to `Snapshot`.
- [x] 5. `cmd/forge/main.go` — wires `WithCoordinator(cleanup.NewPGCoordinator(pgMeta.DB()))`
       when `pgMeta != nil` + startup log (else "in-memory (eval mode)").
- [x] 6. `internal/cleanup/coordinator_pg_test.go` (`//go:build integration`) — two
       `PGCoordinator`s over one testcontainers PG; drives `Tick` concurrently + repeatedly
       at the same due `now`; asserts exactly one run (`len(GetHistory)==1`). PASSES (16s, real PG).
- [x] 7. `go test ./...` + `go vet` green; binary rebuilt; `bash test.sh` 20/20 green.
- [ ] 8. Commit per self-contained unit; update memory.  ← committing now

## Next agenda item (separate workplan when we get there)

Webhooks — on-publish events to HTTP endpoints. Delivery-model decision pending
(synchronous best-effort vs durable via the PG queue; lean = reuse `queue.Queue`).
Not started this session unless time remains after scheduler HA.
