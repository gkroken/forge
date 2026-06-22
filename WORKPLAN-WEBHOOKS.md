# WORKPLAN — Webhooks (on-publish events to HTTP endpoints)

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

## Goal

Emit events on artifact publish to configured HTTP endpoints (CI/Slack/etc.),
HMAC-signed, delivered durably.

## Decision (LOCKED 2026-06-22)

**Delivery model = durable via the PG queue** (user choice). Reuse `queue.Queue`:
enqueue one delivery job per (event × matching subscription); a worker POSTs with
bounded retry. At-least-once, survives restart, replica-safe. Eval mode uses the
in-memory queue (best-effort, same as index-regen jobs today).

### Key architecture constraint
The queue is ONE shared `jobs` table (PG) drained by ONE worker whose `dispatch`
switch (`internal/indexer/indexer.go:75`) **discards unknown job types**. So a
second worker over the same table is wrong (it would grab + discard webhook jobs).
Correct design = register a `webhook.deliver` handler on that single worker. Add a
generic `Worker.Register(typ, handler)` seam; the indexer's `npm.regen` stays
built-in. Bonus: webhook jobs then inherit the existing per-type metrics
(`QueueJobsTotal`) + tasks-ring instrumentation for free.

### Retry policy
Bounded attempts carried in the payload (`Attempt`), self-managed by the handler
which ALWAYS returns nil (so the queue's generic immediate-retry doesn't double-fire).
On failure: re-enqueue `Attempt+1` until `maxAttempts`, then log a dropped delivery.
**Delayed exponential backoff — DONE** (commit pending): added a `visible_after`
primitive to the queue (`EnqueueAfter(ctx, typ, payload, delay)` on the Queue
interface; migration 005 adds `jobs.visible_after`; PG dequeue filters
`visible_after <= now()`; Mem schedules via `time.AfterFunc`). Webhook retries use
equal-jitter exponential backoff (`backoffBase` 2s · 2^(n-1), cap 5m). So a
temporarily-down endpoint gets time to recover and the single worker isn't pinned on
a doomed delivery. `WithBackoff` injects a fast schedule in tests.

### Payload carries subID, not the secret
Enqueue `{subID, event, attempt}`; the worker loads the subscription at delivery
time. This (a) respects a since-disabled/deleted subscription, (b) keeps the HMAC
secret out of the queue table.

## Data model

`webhook.Subscription` (persisted in meta.Store ns `"webhooks"`, key=ID; mirrors
`cleanup.PolicyManager`):
- ID, Name, URL, Secret (HMAC key), Events []string (empty = all),
  Repo (name filter; "" or "*" = all), Enabled, CreatedAt.

`webhook.Event`: Type ("artifact.published"), Repo, Format, Path, Timestamp.

## Phases

### Phase W1 — delivery spine + JSON API (usable via curl, testable) — DONE
- [x] `internal/webhook/webhook.go` — Subscription, Event, Store (CRUD over meta.Store).
- [x] `internal/webhook/sign.go` — `Sign(secret, body) -> "sha256=<hex>"` (HMAC-SHA256).
- [x] `internal/webhook/engine.go` — `Engine{store, q, client}`; `Dispatch(ctx, ev)`
      (match enabled subs → Enqueue `webhook.deliver`); `Handle(ctx, job)` (load sub,
      POST signed body w/ `X-Forge-Signature`+`X-Forge-Event` headers + timeout, bounded retry).
- [x] `indexer.Worker.Register(typ, handler)` seam + ctx-aware `dispatch`.
- [x] `server`: `WithWebhooks(*webhook.Engine)`; registers handler in `WithQueue`
      (WithWebhooks ordered first in main.go); emits `artifact.published` in the publish
      hook (next to `Scheduler.Notify`), off the request path via `go Dispatch`.
- [x] Admin JSON API: `GET/POST /api/v1/webhooks`, `DELETE /api/v1/webhooks/{id}`,
      `POST /api/v1/webhooks/{id}/test` (test-ping). Admin-gated; secret blanked in list.
- [x] `cmd/forge/main.go` — constructs `webhook.New(metaStore, q, nil)`, wires `WithWebhooks`.
- [x] Tests: sign vector; Matches table (event/repo filters, disabled); Dispatch+Handle
      round-trip against `httptest` (signature verifies); disabled-drop; retry cap (race-clean).
- [x] `go test ./...` + vet green; binary rebuilt; `test.sh` 20/20; **live-verified end-to-end**
      (register → PUT artifact → receiver got signed `artifact.published`, HMAC MATCH; test-ping
      `{"ok":true}`; delete 204).

### Phase W2 — admin HTML UI — DONE
- [x] `/ui/admin/webhooks` page (`templates/webhooks.html` + `ui_webhooks.go` +
      `tmplWebhooks`): instrument-panel readouts (Endpoints/Active/Event/Delivery —
      Delivery mirrors queue backend: "durable (Postgres)" vs "in-memory (eval)"),
      endpoints table (Active/Paused pills, Send test + Delete), and an "Add an endpoint"
      form. Inline JS drives the W1 JSON API (fetch). Sidebar nav item ("webhook" icon,
      ActiveNav "webhooks") + route in ui_admin.go. Admin-gated via RequireAdminUI.
- [x] Used `frontend-design:frontend-design` skill — confirmed Foundry is the pinned brief
      (extend, don't replace); applied its copy guidance (operator-side, action-first labels,
      empty state as invitation, errors with direction).
- [x] Live-verified 2× (Playwright screenshot): on-brand, sidebar active, live readouts,
      table populates, form labelled. Full `go test ./...` + `test.sh` 20/20 green; binary rebuilt.

## STATUS: W1 + W2 COMPLETE. Webhooks feature shipped.

## Out of scope (deliberate)
- Policy/audit events — V1 is artifact.published only (extensible via Event.Type).
- OCI `/v2/` publish events — same boundary as cleanup (the four `/repository/` formats first).
