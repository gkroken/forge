# WORKPLAN — Webhooks Hardening (close every deferred gap)

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

The base webhooks feature shipped (WORKPLAN-WEBHOOKS.md: durable delivery, HMAC,
3 event types, exp-backoff retries, admin UI). This plan closes the 12 honest gaps
left behind. **Acceptance for the whole track: every gap below is closed, each with
unit tests + a live end-to-end check; `go test ./...`, `go vet`, `bash test.sh` green.**

## Gap → phase matrix (the definition of "everything covered")

| # | Gap | Phase |
|---|-----|-------|
| 1 | OCI/Docker pushes emit nothing (`/v2/` not hooked) | H1 |
| 2 | Format-native deletes don't emit `artifact.deleted` (npm unpublish, helm/maven delete) | H1 |
| 3 | Manual cleanup runs don't emit `cleanup.completed` | H1 |
| 4 | No `artifact.cached` for proxy cache fills | H1 |
| 9 | No delivery ID for receiver-side dedup | H2 |
| 10 | `Retry-After` on 429 not honored | H2 |
| 12 | No replay protection (timestamp in signature) | H2 |
| 5 | Delivery metrics dishonest (always "success") | H3 |
| 6 | No delivery history/log in the UI | H3 |
| 7 | Dropped deliveries vanish (no dead-letter) | H3 |
| 8 | No edit of subscriptions (create/delete only) | H4 |
| 11 | No SSRF guard on webhook target URLs | H4 |

---

## Phase H1 — Event coverage (everything that should fire, fires)
Acceptance: every artifact-lifecycle action emits the correct event across **all**
formats — publish (incl. OCI), cache-fill, delete (incl. format-native) — and cleanup
emits whether **manual or automated**.

- [x] **#1 OCI publish.** Emit `artifact.published` on a successful OCI manifest PUT
      (`PUT /v2/{name}/manifests/{ref}`). Middleware publish hook extended to match manifest
      PUTs under `/v2/` via `ociManifestRef`; format="oci", path={image}:{ref}. Blob/upload
      PUTs are excluded (no `/manifests/` segment). Live-verified (201 PUT → published).
- [x] **#2 Format-native deletes.** Centralized `artifact.deleted` emit in the middleware for
      `DELETE` to `/repository/` (path carried, format resolved) and OCI manifest DELETE (path
      {image}:{ref}) on 2xx. Covers npm unpublish, helm/maven/cran delete. Admin-API component
      delete keeps its richer emit (different path prefix → no double-fire). Live-verified.
- [x] **#3 Manual cleanup.** Added `Engine.EmitCleanupCompleted(ctx, repo, policy, deleted,
      freed, trigger)` (actor derived from trigger). Called from the scheduler run-hook
      (main.go) AND `handleCleanup`/`handleRunPolicy` on a non-dry run that deleted >0
      (trigger="manual"). Live-verified (manual run, deleted=1, trigger=manual).
- [x] **#4 `artifact.cached`.** New event type added to `AllEventTypes` (→ UI checkboxes).
      `proxy.Config.OnCacheFill(blobKey)` fired by the singleflight leader after a 200 store;
      threaded via `format.Context.OnCacheFill` → server `onProxyCacheFill` → Dispatch. npm
      packument fills (own meta-backed path) emit too. Live-verified (tarball, jar, packument).
- [x] Tests per event (webhook EmitCleanupCompleted, proxy OnCacheFill, server OCI publish/
      delete + repository delete + ociManifestRef table); live-verified each fires. Committed.

## Phase H2 — Delivery semantics & correctness
Acceptance: each delivery carries a stable unique id + signed timestamp; a receiver can
dedup and reject replays; 429 `Retry-After` is respected.

- [x] **#9 Delivery ID.** `NewID()` generated per subscription at Dispatch, stored on the
      delivery job → stable across retries. Sent as `X-Forge-Delivery` and as `id` in the body
      envelope. README documents dedup. Live-verified (id matches in header + body).
- [x] **#12 Replay protection.** `Sign(secret, ts, body)` now HMACs `"{timestamp}.{body}"`;
      `X-Forge-Timestamp` sent; `Verify(...)` helper rejects out-of-tolerance timestamps +
      tampered bodies. Body envelope bumped to `schemaVersion: 2`. README has a Python verifier.
      Live-verified (receiver recomputed signature → sigOK, schema=2).
- [x] **#10 `Retry-After`.** `parseRetryAfter` handles delta-seconds + HTTP-date; `Deliver`
      returns it on 429/503; `Handle` uses `max(expBackoff(n), retryAfter)` clamped to 1h.
- [x] Tests: Sign timestamp vector, `Verify` replay+tamper rejection, delivery-id stable across
      a 429 retry + Retry-After honoured (≥1s gap), `parseRetryAfter` table. Committed.

## Phase H3 — Operability & observability
Acceptance: an operator can see per-endpoint recent deliveries (status, code, attempts,
time), failed + dropped deliveries are visible (dead-letter), and metrics reflect real outcomes.

- [x] **#5 Honest metrics.** `forge_webhook_deliveries_total{result=success|failed|dropped}`
      added to obs.Metrics; the engine increments it per attempt via `WithMetrics` callback
      (keeps webhook pkg free of an obs dependency). No longer relies on the always-nil queue
      result. Live-verified at `/metrics` (1 success / 4 failed / 1 dropped).
- [x] **#6/#7 Delivery history + dead-letter.** `webhook.History` (meta ns "webhook-deliveries",
      capped 50/sub, mutex-guarded, newest-first): id, event, repo, status, httpCode, attempt,
      error, ts. Terminal failure flagged `dropped` (the dead-letter). Read API
      `GET /api/v1/webhooks/{id}/deliveries`; history dropped when the sub is deleted.
- [x] **UI.** Per-endpoint "Delivery trace" drawer (frontend-design skill, Foundry instrument
      voice): a mini readout (delivered/failed/dropped), status pills, code/attempt/event/when/
      detail columns, and a "Dead-letter only" filter. Moved all page JS to `/ui/static/
      webhooks.js` (the old inline JS violated the page CSP) with data-attr event delegation.
- [x] Tests: success record + metric; dead-letter on exhaustion (4 failed + 1 dropped, metrics
      match). API + UI live-verified (screenshot of the trace panel). Committed.

## Phase H4 — Management & security
Acceptance: subscriptions are full CRUD (edit URL/secret/events/filter/enabled); webhook
targets are validated against an SSRF policy at create AND delivery time.

- [x] **#8 Edit.** `PUT /api/v1/webhooks/{id}` + `Store.Update` (blank secret preserves the
      stored one; CreatedAt preserved). UI edit mode: per-row "Edit" prefills the form (name/
      URL/repo/events from row data-attrs), secret write-only ("leave blank to keep"), Save
      changes / Cancel. Live-verified (rename + secret-preserve 200; UI screenshot).
- [x] **#11 SSRF guard.** `webhook.SSRFGuard` blocks loopback / link-local / private / ULA /
      unspecified / multicast / `169.254.169.254`. `ValidateURL` at create/update; `Control`
      (a `net.Dialer.Control` hook on the engine's HTTP transport) re-checks the concrete dialed
      IP, defeating DNS rebinding. Default-deny; `WEBHOOK_ALLOW_PRIVATE` escape hatch (main.go).
- [x] Tests: update round-trip (secret-preserve + missing-id error), SSRF block table
      (loopback/metadata/private blocked, public allowed, allow-private override, bad scheme),
      dial-time Control rebinding block, API create/edit rejection. Live-verified. Committed.

## Constraints
Go stdlib only (flag any new dep). Keep `go test ./...` + `test.sh` green. Commit per
self-contained unit. Rebuild the binary after CSS/JS changes. Don't touch vuln scanning.

## Done =
All 12 rows above checked, each backed by tests + a live check; full suite + test.sh green;
WORKPLAN-WEBHOOKS.md "Out of scope" list reduced to only the genuinely-future items
(OCI nuance edge cases, policy-violation events that depend on vuln scanning).
