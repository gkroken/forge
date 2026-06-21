# forge — Cleanup: On-Publish Trigger & Retention Completeness

Source of truth for cleanup-policy work. The 24h `cleanup.Scheduler` is the coarse
safety net; this plan adds the **responsive path** (run a policy when an artifact
lands) and closes the long-standing retention gaps (`lastDownloadedDays` no-op,
lexicographic version sort, proxy caches never swept).

Constraints carry through every phase: **Go stdlib only**, `go test ./...` stays
green, commit per self-contained unit, new routes added to
`security_headers_test.go`.

---

## 0. Why

`keep-N` / `keep-releases-only` should react when a new version is published, not
only on the nightly sweep (push v2 under `keep-2` → runs, deletes nothing; push
`snapshot-4` → prunes `snapshot-1`). Standard pattern in Nexus/Artifactory.

Design decisions already settled:
- **Trigger at the server middleware** on a 2xx write to `/repository/` (and
  `/v2/` for OCI). `cleanup.Run` re-enumerates the repo, so the trigger only needs
  "repo R was written" — format handlers never import `cleanup`.
- **Async + debounced.** Never inside the upload request. A Maven version push is
  many PUTs (`.jar`/`.pom`/`.sha1`…); coalesce to one run per cooldown window.
- **Gated by an explicit per-policy opt-in** (`RunOnPublish`, default off):
  destructive automation should be discoverable and per-policy granular, and it
  future-proofs a richer trigger model (publish / schedule / download).

---

## 1. Definition of Done

- Publishing to a hosted repo whose assigned policy has `RunOnPublish` set fires a
  single debounced cleanup run; history is recorded; the upload path adds no
  latency and can never be failed by the sweep.
- A freshly published higher version is **never** pruned by its own publish (semver
  sort fixed — Phase 0).
- `lastDownloadedDays` deletes artifacts not downloaded in N days, backed by real
  per-artifact download timestamps (Phase 2).
- Proxy caches can be swept by time/last-download rules (Phase 3).
- All new routes/fields covered by tests; `internal/cleanup` and `internal/server`
  coverage not regressed.

---

## Phase 0 — Semver-aware version sort *(prerequisite)*

`cleanup.go:154` and `cleanup.go:393` sort versions with lexicographic
`version < version`, so `1.10.0` sorts below `1.9.0`. Under `keep-2`, pushing
`1.10.0` into `{1.8.0, 1.9.0}` deletes the just-pushed `1.10.0`. Nightly this is an
annoyance; **on-publish it is an instant, dramatic failure** — fix first.

- Add `compareVersions(a, b string) int` in the cleanup pkg: split on `.`/`-`,
  compare dotted segments numerically when both numeric, lexicographically
  otherwise; release > snapshot/pre-release on tie. Stdlib only.
- Swap both insertion-sort comparators to use it.
- **Test:** `keep-2` over `{1.8.0, 1.9.0, 1.10.0}` keeps `1.9.0` + `1.10.0`,
  deletes `1.8.0`; existing tests stay green.
- **Commit:** `cleanup: semver-aware sort for keep-N (1.10.0 was pruned before 1.9.0)`

---

## Phase 1 — On-publish trigger *(core deliverable)*

### 1a. Model
- `RunOnPublish bool` on `cleanup.NamedPolicy` (`json:"runOnPublish,omitempty"`).

### 1b. Form
- Checkbox in `cleanup_policy_form.html`; parse in both branches of
  `processCleanupPolicyForm`; pre-check on edit; add to `policySummary`
  ("Runs on publish").

### 1c. Debounced runner
- Extend `cleanup.Scheduler` (already owns repos/policies/blob/meta + `lastRun`):
  add `Notify(repoName string)` — non-blocking, coalesces per-repo within a
  cooldown window, fires `Run` in a goroutine, records history.
- Factor the existing `RunDue` body into a shared `runOne(repo)` to avoid
  duplicating the Run + RecordRun logic.

### 1d. Wiring
- Server stays format- and policy-agnostic: middleware calls
  `s.Scheduler.Notify(repoName)` on a 2xx write to `/repository/` (next to the
  existing blob-walk trigger) and `/v2/`. `Notify` does the lookup and gates on
  `Kind == Hosted && policy.RunOnPublish`.

### Tests
- Coalescing fires once per window; `RunOnPublish=false` → no-op; proxy repo
  skipped.
- **Commits:** (a) model + form, (b) runner + wiring.

---

## Phase 2 — `lastDownloadedDays` → download tracking

The field exists but is dropped in `ToCleanupPolicy`; only a Prometheus per-repo
counter exists, no per-artifact timestamp. (Original intent — see the now-deleted
`WORKPLAN-UI.md` §W3a — was always to back it with download-time tracking.)

- **2a.** Persist per-artifact `downloadedAt` in `meta.Store` (new namespace, key
  = repo+path). Write from the middleware's existing GET-200 artifact branch;
  throttle (only update if stale by >1h) so hot downloads don't hammer meta.
- **2b.** Thread a download-time lookup into `Run`/`DryRun`; a version becomes a
  candidate when last-download (fallback: publish time) is older than N days. Stop
  dropping the field in `ToCleanupPolicy`.
- **2c.** Drop the "coming soon" / no-op labels in `policy.go` and `ui_cleanup.go`.
- **Commits:** tracking store → cleanup wiring → UI label. *(Largest phase.)*

---

## Phase 3 — Proxy cache eviction

Cleanup hard-skips `Kind != Hosted`, yet proxy caches are where unbounded growth
actually hurts.

- Allow assigning **time-based / last-downloaded** policies to proxy repos; `Run`
  evicts cached blobs for those rules only (count-based stays hosted-only —
  meaningless for a cache). Relax the `Hosted` guard in `Scheduler` / `Notify` /
  `Reclaimable` for time-based policies.
- Depends on Phase 2 for the download-age signal; do after.
- **Commit:** `cleanup: time-based eviction for proxy caches`

---

## Phase 4 — Live verify + memory

Build, seed `/tmp/forge-demo`, verify against the running server:
- `keep-2` push-`v2` deletes nothing.
- push-`snapshot-4` prunes `snapshot-1`.
- push-`1.10.0` is **not** deleted (Phase 0).
- `RunOnPublish=off` doesn't fire.

Update memory with the shipped behaviour.

---

## Sequencing

**This session:** Phases **0 + 1** (the on-publish ask, done safely).
**Confirm scope before starting:** Phases **2 + 3** are genuinely new capability;
in the plan as agreed, executed separately.
