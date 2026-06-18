---
name: project-ui-workplan
description: UI workplan status as of 2026-06-18. Foundry shell landed. W1 done. W3 plan confirmed (3 commits). Source of truth: WORKPLAN-UI.md.
metadata:
  type: project
---

UI workplan lives in `WORKPLAN-UI.md`. `WORKPLAN.md` is prototype-era, NOT authoritative for UI.

**Why:** Accurate status needed to plan next UI work.

**How to apply:** Check WORKPLAN-UI.md §1 status table before assuming anything is done.

---

## Phase summary (legacy U0–U3 — all complete)

U0 (auth guard), U1 (handler test suite), U2 (token mgmt, search, upload, component detail), U3 (dark mode, nav, icons, proxy browse, dep links) — all shipped. These phases are closed.

---

## Foundry design — what landed (branch feature/foundry-remaining-tabs, commit ed687ab)

214px left sidebar shell, steel-blue accent `#3a6ea5` (W1 done), Dashboard, Tokens, Cleanup, Observability pages all wired. Not yet merged to main.

**CSS sidebar bug fixed (same session):** global `nav` selector was leaking dark `--nav-bg` into `<nav class="sidebar-nav">`. Fix: all top-nav rules scoped to `body > nav`. Also added `height: 100%` to `.admin-sidebar` so `margin-top: auto` works on footer.

---

## Open work (W1–W5 from WORKPLAN-UI.md §3)

Build order: W1(done) → W2a → W5 → W3a → W3b → W2b → W4

| Item | Status | Notes |
|------|--------|-------|
| W1 — steel-blue accent | ✅ done | `--accent: #3a6ea5` in light + dark |
| W2a — audit log ring buffer | ❌ open | `internal/obs.AuditLog`; feeds Dashboard activity + Observability audit table |
| W2b — rate chart (real vs representative) | ❌ decision pending | Currently hardcoded bell curve |
| W3 — Cleanup U4 (full named-policy system) | ❌ open — plan confirmed | See W3 plan below |
| W4 — Dashboard completeness | ❌ open | Stored bytes KPI + real scheduler task list. Blocked on W3a scheduler exposure |
| W5 — Per-version publish timestamps | ❌ open | npm (packument `time` map) + Helm (chartRecord.UploadedAt); CRAN/Maven leave blank |

---

## W3 plan (confirmed 2026-06-18, ready to implement)

**What exists today:**
- `repo.go:105` — `Repository.CleanupPolicy *CleanupPolicy` (inline, optional)
- `cleanup.go:36` — `Run(r repo.Repository, b, m)` reads `r.CleanupPolicy`
- `scheduler.go:49` — reads `r.CleanupPolicy` directly per repo
- `ui_admin.go:484` — `parseCleanupPolicy()`, called at lines 237 + 282
- `admin.go:249` — `handleCleanup()` calls `cleanup.Run(rp, s.Blob, s.Meta)`
- `internal/cleanup/` — only has `cleanup.go`, `cleanup_test.go`, `scheduler.go`, `scheduler_test.go`
- `Server` struct — no `Cleanup` field yet
- **`main.go` does NOT seed any repos with inline `CleanupPolicy`** — only `cleanup.NewScheduler(mgr, blobStore, metaStore).Start(workerCtx)` is wired there

**Three commits:**

**Commit 1 — additive (no existing code changes):**
- `internal/cleanup/policy.go` — `NamedPolicy`, `PolicyManager` (CRUD to meta.Store, namespace `cleanup-policies`)
- `internal/cleanup/dryrun.go` — `DryRun()`: same logic as `Run()`, returns `[]Candidate` instead of deleting
- `internal/cleanup/history.go` — `CleanupRun`, `RecordRun()`, `GetHistory()` (keep last 20 per repo)
- New API routes: `GET/POST/DELETE /api/v1/cleanup-policies`, `GET /api/v1/cleanup-policies/{name}`, `POST /api/v1/repos/{name}/cleanup?dry=true`
- Tests for all new code

**Commit 2 — breaking (must leave `go test ./...` green):**
- Remove `Repository.CleanupPolicy *CleanupPolicy`, add `CleanupPolicyName string`
- Change `cleanup.Run()` to take `repoName string, p *CleanupPolicy, b, m` (decoupled from repo model)
- `Scheduler` gains `*PolicyManager` dep, resolves policy by name at run time
- Add `Cleanup *cleanup.PolicyManager` to `Server` struct; wire in `main.go`
- Delete `parseCleanupPolicy()` from `ui_admin.go`; add `cleanupPolicyName` dropdown handling
- Update `handleCleanup()` to resolve named policy
- Update all tests (`cleanup_test.go`, `scheduler_test.go`, `admin_test.go`)
- Data migration: none needed — Go JSON decoder silently ignores removed `cleanupPolicy` field

**Commit 3 — UI:**
- Rewrite `ui_cleanup.go` — list named policies, CRUD handlers, per-repo run panel
- New templates: `cleanup_policy_form.html` (create/edit), `cleanup_run.html` (dry-run + history panel)
- Rewrite `cleanup_policies.html` — named policy table, CRUD links
- Update `admin_repo_form.html` — replace inline fieldset with `<select>` dropdown (hosted repos only, extend `syncKind()`)
- Add all new routes to `security_headers_test.go`

**Deferred:** `LastDownloadedDays` rule field + UI included but blob-read timestamp tracking deferred — rule silently no-ops on artifacts without download timestamp.

---

## Key design decisions (locked)

- Auth: `RequireAdminUI` checks `Authorization: Bearer` then `forge_token` HttpOnly SameSite=Strict cookie
- Session cookie: `auth.UISessionCookie = "forge_token"`
- CSP: `script-src 'self' https://unpkg.com` — no additional external scripts
- Dark mode: token-only via `:root` + `[data-theme="dark"]` overrides; no per-component dark rules
- All assets embedded via `//go:embed templates static`; CSS rebuild requires `go build` + restart
- `cssVer` is a content hash computed at startup — browser cache-busts on rebuild automatically
- No build step; stdlib + htmx 2.0.3 only
</content>
</invoke>