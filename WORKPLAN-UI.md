# forge Web UI — Workplan

Source of truth for all UI work under `internal/server`. The backend
`WORKPLAN.md` is prototype-era; do not treat it as authoritative for UI.

---

## 0. Design direction (adopted)

**Foundry shell** — left sidebar (214 px rail), steel-blue accent `#3a6ea5`,
light surfaces `#fbfcfd / #f7f9fb`, dark mode via CSS token overrides only.
Reference mockups: `new_design_docs/forge-app-ui-mockup/project/design/`.

Design files are **design intent, not shippable code**. All hard constraints
from §6 apply when porting anything.

---

## 1. Current state

All templates, the admin sidebar shell, and Go handlers from the design are
landed. The following is live and functional:

| Surface | Route | Status |
|---|---|---|
| Sidebar shell | `admin_shell.html` wraps all admin pages | ✅ live |
| Repositories list (browse) | `/ui/` | ✅ live |
| Repositories list (admin) | `/ui/admin/` | ✅ live |
| Repo create / edit | `/ui/admin/repos/new`, `/ui/admin/repos/{name}/edit` | ✅ live |
| Browse & search | `/ui/repos/{name}`, `/ui/search` | ✅ live |
| Component detail | `/ui/repos/{name}/{component}` | ✅ live |
| Token management | `/ui/admin/tokens` | ✅ live |
| Access view | `/ui/admin/access` | ✅ live |
| Upload | `/ui/repos/{name}/upload` | ✅ live |
| Dashboard — KPI row | `/ui/dashboard` | ✅ live (Prometheus counters) |
| Dashboard — format bars | `/ui/dashboard` | ✅ live (repo counts) |
| Dashboard — request chart | `/ui/dashboard` | ⚠️ placeholder (hardcoded bell curve) |
| Dashboard — recent activity | `/ui/dashboard` | ⚠️ placeholder (always empty) |
| Dashboard — background tasks | `/ui/dashboard` | ⚠️ placeholder (always empty) |
| Dashboard — stored bytes | `/ui/dashboard` | ⚠️ placeholder (shows `—`) |
| Observability — KPI row | `/ui/admin/observability` | ✅ live (Prometheus counters) |
| Observability — status breakdown | `/ui/admin/observability` | ✅ live (from Prometheus labels) |
| Observability — rate chart | `/ui/admin/observability` | ⚠️ placeholder (hardcoded pattern) |
| Observability — audit log | `/ui/admin/observability` | ⚠️ placeholder (always empty) |
| Cleanup — policy list | `/ui/admin/cleanup-policies` | ✅ live (reads `CleanupPolicy` on repos) |
| Cleanup — scheduled tasks | `/ui/admin/cleanup-policies` | ⚠️ placeholder (3 hardcoded rows) |
| Cleanup — CRUD + dry-run | (planned) | ❌ not built |

---

## 2. Definition of Done

GA for the UI is reached when **all** hold:

- No UI route reads or mutates anything without going through the same authz
  decision the JSON / `/repository/` routes enforce.
- Every UI route **and every htmx fragment** has a handler test.
- Security headers asserted on every route (`security_headers_test.go` green;
  new routes must be added to its route table).
- Each user-facing surface is usable end-to-end (browse, search, token mgmt,
  upload, cleanup). The upload surface is covered by a click-through that
  drives a real publish and verifies the artifact appears in browse.
- Runs with stdlib + htmx only, no build step, all assets embedded.
  CSP stays `script-src 'self' https://unpkg.com`.

---

## 3. Open work

### W1 — Accent colour (decision, ~5 min)

Adopt the Foundry steel-blue `#3a6ea5` (proposed) or keep GitHub-blue
`#0969da` (current). One change: `--accent` and `--accent-dark` in
`style.css`, plus `--accent-subtle` and `--accent-subtle-dark`.

**Exit:** `--accent` token updated (or explicitly left as-is); committed.

---

### W2 — Observability data feed

The audit log and rate chart currently show no real data.

#### W2a — In-memory audit log ring buffer

Add `internal/obs.AuditLog` — a fixed-size ring buffer (e.g. 500 entries)
of write and auth events. Each entry: timestamp, actor (token description or
`anonymous`), method, path, status code.

Wire it into the server middleware: append to the ring on every
`POST/PUT/DELETE/PATCH` request and on every auth failure (401/403).

Expose it to the two consumers:
- **Observability page** — `AuditLog []auditRow` in `observabilityPage`; last
  N entries rendered in the audit table (currently shows "No in-memory log
  yet").
- **Dashboard "Recent activity"** — last 5 write events surfaced as
  `activityRow` entries.

**Exit:** audit log populates on real requests; Observability audit table and
Dashboard activity panel show live entries; handler tests cover both pages.

#### W2b — Rate chart (decide: real or representative)

The 24h / 32-bar charts are currently a hardcoded bell curve. Options:

- **Keep representative** — document the bars as cosmetic; no further work.
- **Real bucketed counters** — add a 24-slot ring of per-hour request counts
  to `internal/obs`; bump the current-hour bucket on each request; serve from
  that on dashboard and observability page loads.

Decision required before building. If keeping representative, mark closed.

---

### W3 — Cleanup Phase U4 (full named-policy system)

The current cleanup page is a read-only list of repos that have an inline
`CleanupPolicy`. The full design treats policies as first-class named objects.

#### W3a — Backend

- **Named policy store**: `CleanupPolicy` manager persisting named policies to
  `meta.Store` under a `cleanup-policies` namespace. Each policy: `Name`
  (slug), `Description`, plus the existing rule fields (`KeepVersions`,
  `KeepReleasesOnly`, `DeleteSnapshotsDays`, `DeleteOlderThanDays`,
  `Interval`). Add `LastDownloadedDays int` (delete artifacts not downloaded
  in N days — requires download-time tracking on blob reads; wire in blob
  middleware and store alongside upload timestamps).
- **Repo → policy assignment**: replace `Repository.CleanupPolicy
  *CleanupPolicy` with `Repository.CleanupPolicyName string`. Scheduler
  resolves the named policy at run time.
- **Dry-run mode**: `cleanup.DryRun(r, b, m) (Result, error)` — same logic as
  `Run` but deletes nothing. `Result` gains `Components []CleanupCandidate`
  (name, version, size, age).
- **Run history**: after each `Run` (scheduler or manual), append a
  `CleanupRun` record to meta (`{repoName}:cleanup:history`): timestamp,
  policy name, deleted count, freed bytes. Keep last 20 per repo.
- **API**: `GET/POST/DELETE /api/v1/cleanup-policies`,
  `GET /api/v1/cleanup-policies/{name}`,
  `POST /api/v1/repos/{name}/cleanup?dry=true`.

#### W3b — UI

**`/ui/admin/cleanup-policies`** (update existing page):
- Replace hardcoded scheduled-task cards with real policy rows.
- Table: Name | Description | Rules summary | Assigned repos | Actions.
- "New policy" button → `/ui/admin/cleanup-policies/new`.
- Per-row Edit / Delete (delete blocked if policy is assigned to any repo,
  with a message listing them).

**`/ui/admin/cleanup-policies/new` and `.../edit/{name}`** — policy form:
- Name (immutable slug after create) + Description.
- Toggleable rule cards: Keep last N versions · Delete older than N days ·
  Delete snapshots older than N days · Delete not downloaded in N days ·
  Keep releases only.
- Schedule: interval string (`24h`) or leave blank for manual-only. Show next
  scheduled run time as a hint once saved.

**`/ui/admin/repos/{name}/cleanup`** — per-repo run panel:
- Assigned policy summary in plain English.
- **Dry-run** button: `POST /api/v1/repos/{name}/cleanup?dry=true`, renders
  candidate table (Component | Version | Size | Age | Reason) via htmx swap.
  Confirm button below executes the real run.
- **Run history** table: Timestamp | Policy | Deleted | Freed | Duration —
  last 20 runs. Empty state: "No cleanup runs recorded yet."
- **Run now** button with confirmation dialog.

**Repo edit form** — replace the inline cleanup fieldset with:
- A "Cleanup policy" dropdown (hosted repos only; hidden for proxy/group).
- A "Manage policies" link → `/ui/admin/cleanup-policies`.

#### W3 interaction requirements

- Policy section on repo form only appears when `kind === hosted`;
  `syncKind()` in the form JS already handles hiding sections — extend it.
- Dry-run table renders via htmx swap (`hx-post`, target `#dryrun-results`).
- Run history refreshes after a manual run (htmx swap the history block in
  the response).
- All mutations require `RequireAdminUI`.
- New routes added to `security_headers_test.go`.

**Exit:** named cleanup policies are CRUD-able; repo edit shows dropdown for
hosted repos only; dry-run renders a candidate table; run history shows last
20 runs; all new routes have handler tests; `internal/server` coverage ≥ 80%;
security headers test green.

---

### W4 — Dashboard completeness

Two remaining placeholder panels after W2 lands:

- **Stored bytes** KPI: walk the blob store root at dashboard load (or cache
  the result for 60 s) and populate the `Stored` field. Cap the walk at a
  reasonable depth to keep latency low; show `—` on error.
- **Background tasks**: wire the dashboard to the real `cleanup.Scheduler`
  state — expose a `Tasks() []ScheduledTask` method that returns each repo's
  next-run time and last-run status; render in the "Background tasks" panel.

**Exit:** both panels show real data; handler test covers the dashboard page.

---

### W5 — Per-version publish timestamps (#27)

The version list on the component detail page shows no publish date per
version.

- Add `PublishedAt time.Time` to `format.VersionInfo`.
- Populate in each format's `Inspect`:
  - npm — from packument `time[version]` map (already in memory).
  - Helm — from `chartRecord.UploadedAt` (already stored per version).
  - CRAN / Maven — leave as zero (no reliable per-version source).
- Render as a small secondary date next to each version in `component.html`.

**Exit:** npm and Helm component pages show a publish date per version; CRAN
and Maven show nothing (not a blank cell — omit the column when all zeros).

---

## 4. Build order

1. **W1** — accent token (5 min, any time)
2. **W2a** — audit log ring buffer (unblocks W2-dependent panels)
3. **W5** — per-version timestamps (independent, low risk)
4. **W3a** — cleanup backend (W3b is blocked on this)
5. **W3b** — cleanup UI
6. **W2b** — rate chart decision + impl (if building real counters)
7. **W4** — dashboard completeness (W4 background tasks blocked on W3a scheduler exposure)

---

## 5. Constraints (carry through every phase)

- Go stdlib only; no new Go dependency without explicit approval.
- No build step; CSS/JS usable as-is.
- Assets embedded under `internal/server/templates/` or `.../static/`.
- htmx only for client interactivity; additional JS must justify itself
  against CSP `script-src 'self' https://unpkg.com`.
- Dark mode is token-only: new colours as `:root` custom properties with a
  `[data-theme="dark"]` + `prefers-color-scheme` override. No per-component
  dark rules.
- `go test ./...` stays green at all times.
- New routes must be added to the route table in `security_headers_test.go`.
