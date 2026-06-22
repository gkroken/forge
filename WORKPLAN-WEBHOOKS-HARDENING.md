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

- [ ] **#9 Delivery ID.** Generate a delivery id at Dispatch (stable across retries; reuse
      `newID()`), put it on the payload, send as `X-Forge-Delivery`. Document dedup for receivers.
- [ ] **#12 Replay protection.** Sign `timestamp + "." + body` (Stripe-style); send
      `X-Forge-Timestamp`. Update `Sign`/verification + README example; bump a payload
      `schemaVersion`. (Pre-prod, so changing the signature contract is fine — note it.)
- [ ] **#10 `Retry-After`.** Parse `Retry-After` (delta-seconds or HTTP-date) on 429/503;
      `Deliver` surfaces it; `Handle` uses `max(expBackoff(n), retryAfter)` for the re-enqueue delay.
- [ ] Tests: signature+timestamp vector, replay rejection (verifier helper), delivery-id
      stable across retries, Retry-After honored. Commit.

## Phase H3 — Operability & observability
Acceptance: an operator can see per-endpoint recent deliveries (status, code, attempts,
time), failed + dropped deliveries are visible (dead-letter), and metrics reflect real outcomes.

- [ ] **#5 Honest metrics.** Add `forge_webhook_deliveries_total{result=success|failed|dropped}`
      (+ maybe latency); increment in Deliver/Handle. Stop relying on the always-nil queue result.
- [ ] **#6/#7 Delivery history + dead-letter.** Persist recent deliveries per subscription
      (meta.Store ns, capped ring per sub: id, event type, status, http code, attempt, ts,
      error). Dropped-after-max records flagged `dropped` (the dead-letter). Read API
      `GET /api/v1/webhooks/{id}/deliveries`.
- [ ] **UI.** Per-endpoint deliveries panel/drawer (recent attempts + status pills + a
      "dropped" filter). Use the `frontend-design:frontend-design` skill; Foundry instrument voice.
- [ ] Tests: metrics increment by outcome; history records success/failure/dropped; API +
      UI render. Live-verify. Commit.

## Phase H4 — Management & security
Acceptance: subscriptions are full CRUD (edit URL/secret/events/filter/enabled); webhook
targets are validated against an SSRF policy at create AND delivery time.

- [ ] **#8 Edit.** `PUT /api/v1/webhooks/{id}` + `Store.Update` (preserve secret if blank on
      edit) + an edit form/route in the UI (prefilled; secret write-only).
- [ ] **#11 SSRF guard.** Reject targets resolving to loopback / link-local / private /
      unspecified / multicast / cloud-metadata (169.254.169.254) unless an explicit allowlist
      env permits. Guard at create/update AND at dial time (custom `http.Transport.DialContext`
      `Control` to defeat DNS rebinding). Default-deny private ranges; `WEBHOOK_ALLOW_PRIVATE`
      escape hatch for internal-only deployments.
- [ ] Tests: update round-trip (secret-preserve), SSRF block table (loopback/metadata/private
      blocked, public allowed, allowlist override), dial-time rebinding block. Live-verify. Commit.

## Constraints
Go stdlib only (flag any new dep). Keep `go test ./...` + `test.sh` green. Commit per
self-contained unit. Rebuild the binary after CSS/JS changes. Don't touch vuln scanning.

## Done =
All 12 rows above checked, each backed by tests + a live check; full suite + test.sh green;
WORKPLAN-WEBHOOKS.md "Out of scope" list reduced to only the genuinely-future items
(OCI nuance edge cases, policy-violation events that depend on vuln scanning).
