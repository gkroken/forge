---
name: project-ui-workplan
description: Foundry UI redesign status as of 2026-06-20. F0–F3 + BE-A/B/C/D shipped. WORKPLAN.md §10 is authoritative. Next: BE-E/F (parallel) + BE-G → F4/F2.
metadata:
  type: project
---

The UI redesign plan lives in **`WORKPLAN.md §10`** (not any separate file).
Branch: `feature/foundry-remaining-tabs` — not yet merged to main.

**Why:** Full Foundry redesign (Forge Admin UI.dc.html + remaining tabs mockup).
WORKPLAN.md §10 contains: color tokens, what changed, phase specs, and three
gap analyses (§10.5 repo config, §10.6 dashboard/observability).

**How to apply:** Before starting any UI or metrics work, read §10.4 for the
active phase and §10.5/§10.6 for the gap analyses — they specify data sources
and endpoints at the level needed to implement without ambiguity.

---

## Phase status (Foundry UI redesign — ALL phases shipped as of 2026-06-21)

The Foundry redesign (F0–F4 + BE-A…G + F2-charts) is complete. Active cleanup
work now lives in `WORKPLAN-CLEANUP.md`, not here.

| Phase | Status | What it covered |
|---|---|---|
| F0 — Font bundle | ✅ done | IBM Plex Sans/Mono + Material Symbols as go:embed static assets |
| F1 — CSS + shell | ✅ done | Foundry palette, sidebar, topbar, KPI card slots |
| F2 — Admin pages | ✅ done | Dashboard, Observability, Cleanup, Tokens & Access reskin |
| BE-A — Metrics + cleanup | ✅ done | Blob walker, latency histogram, cleanup PolicyManager, token Owner/LastUsed |
| BE-B — Browse data | ✅ done | Artifact count, file size, PublishedAt all formats, download counter, CB health, tree API |
| F3 — Repository pages | ✅ done | Repos list new columns, global 3-panel Browse, repo config tab strip |
| BE-C — Users + Roles | ✅ done | User model (bcrypt), user CRUD, Roles CRUD, Users/Roles tabs in Tokens & Access, username/password login |
| BE-D — Repo model ext | ✅ done | Enabled, BlobStore, ContentMaxAge, MetadataMaxAge, NegativeCache, AutoBlock, TimeoutSecs, Retries, QuotaGB |
| BE-E — Proxy wiring | ✅ done | proxy.ConfigForRepo() |
| BE-F — Cache metrics | ✅ done | Per-repo hourly hit/miss ring buffer, cache-stats + invalidate endpoints |
| BE-G — System metrics | ✅ done | Global ring buffers, status counters, task API, store capacity, retry counter, DB latency EMA |
| F4 — Repo config UI | ✅ done | `ee07631` — Settings form new fields, Content/Access/Activity tabs, right rail, action buttons |
| F2 (charts) | ✅ done | `402b64f` — Dashboard 24-bar chart, system tasks panel, Observability req/p95 chart, status breakdown |

---

## What shipped (2026-06-19 session)

- **Global browse**: all repos as collapsible nodes in one left pane; format-aware
  expansion (Maven → folder tree, others → flat searchable list). URL syncs via
  pushState. `/ui/repos/{name}` now redirects to `/ui/browse/{name}`.
- **Repo list**: clicking a repo name goes to config/settings page, not browse.
- **Collapse bug fixed**: CSS `display:none` default + `.expanded > .browse-repo-content`
  pattern (was broken because `:empty` doesn't re-hide after content loads).
- **Upload button** on hosted repo nodes in browse header.
- **Gap analyses written** into WORKPLAN.md §10.5 (repo config) and §10.6
  (dashboard + observability). BE-D through BE-G phases defined.

## What shipped (2026-06-20 session)

- **BE-C — Users + Roles**: `auth.UserStore` (bcrypt passwords), `auth.RoleStore`
  (predefined Reader/Publisher/Administrator + custom), user CRUD API at
  `/api/v1/users` + `/api/v1/roles`, Users + Roles tabs in Tokens & Access admin
  page, username/password login flow in `ui.go` + `login.html`.
- **BE-D — Repository model extension**: 9 new fields on `Repository` (Enabled,
  BlobStore, ContentMaxAge, MetadataMaxAge, NegativeCache, AutoBlock, TimeoutSecs,
  Retries, QuotaGB). `Enabled=false` → 503 in `handleRepo` + `handleOCI`. Backward
  compat: old JSON without `"enabled"` defaults to `true`; legacy `"proxyTTL"` copied
  into `ContentMaxAge`. Admin form edit preserves BE-D fields not yet in the form.

---

## Key design decisions (locked this session)

**Browse scope**: shows only locally stored artifacts (blob.Store + meta.Store).
Proxy repos show what's been cached, not upstream catalog. Proxy nodes need a
"Showing locally cached artifacts" subtitle (implement in F4).

**Repo config tabs:**
- *Content*: package+version list with delete and expire-cache actions per version.
  Admin management surface (not read-only like Browse). Extend DELETE to all formats;
  new `DELETE /api/v1/repos/{name}/cache/{pkg}/{ver}` expire endpoint.
- *Access*: `{principal, principal_type, role}` binding table. SSO-safe — new auth
  types add `principal_type` values without changing the table shape. Pre-BE-C:
  token scope only. Post-BE-C: users + groups. New `GET/PUT /api/v1/repos/{name}/access`.
- *Activity*: audit log filtered to this repo via `?repo=` query param on existing
  audit endpoint. No new storage.

**Repo config Settings form new fields**: all new fields on `Repository` use pointer
types so nil = server default. `ProxyTTL` kept as legacy JSON alias for `ContentMaxAge`.
`Enabled bool` false → 503 enforced in routing before format handler dispatch.

---

## Recommended build order

```
BE-C  (Users + Roles — unblocks Access tab and Users/Roles UI)
  ↓
BE-D  (repo model extension)
  ├── BE-E  (proxy per-repo wiring)   ← parallel
  └── BE-F  (per-repo cache metrics)  ← parallel
  ↓
BE-G  (global system metrics — fully parallel with BE-D/E/F)
  ↓
F4    (repo config UI: Settings form + tabs + right rail)
F2-charts  (dashboard 24-bar + tasks panel, obs chart — needs BE-G)
```

---

## Locked technical decisions (carry forward)

- CSP: `script-src 'self' https://unpkg.com` — all JS in external files, no inline handlers
- All JS uses event delegation (no inline onclick/oninput)
- Charts: inline SVG `<rect>` elements — no external charting library
- Static files served at `/ui/static/` prefix
- `admin_shell.html` layout: `{{block "content-class"}}` overrides the wrapper class
- `admin-browse` CSS class for full-bleed 3-panel (no admin-content padding)
- Format-aware browse dispatch keyed on `data-format` attribute (maven → tree, others → flat)
