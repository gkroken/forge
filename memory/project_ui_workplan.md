---
name: project-ui-workplan
description: UI workplan status — U0–U3 complete. #22 done (CRAN+Helm proxy browse), #26/#27 open. Source of truth: WORKPLAN-UI.md.
metadata:
  type: project
---

UI workplan lives in `WORKPLAN-UI.md`. WORKPLAN.md is prototype-era and NOT authoritative for UI work.

**Why:** Accurate status is needed to plan next UI work.

**How to apply:** Check the status column in the gap table before assuming a feature is done.

---

## Phase summary (verified against codebase 2026-06-07)

| Phase | Status | Notes |
|-------|--------|-------|
| U0 | ✅ COMPLETE | Auth guard on all `/ui/admin/*` mutations; login/logout; `RequireAdminUI` cookie+header check |
| U1 | ✅ COMPLETE | Full handler+fragment test suite; ≥80% coverage on UI handlers |
| U2 | ✅ COMPLETE | Token mgmt, search filters, upload, access view. Component detail fully enriched (#12–#17). |
| U3 | ✅ COMPLETE | Cache-busting, dark mode, nav hx-boost, admin breadcrumb, format icons, sortable columns, proxy timestamps, proxy browse, dep links, health indicator all shipped. |

---

## Open gaps (❌)

- **#26** Version-specific component pages (`/ui/repos/{repo}/{pkg}/{version}`) — not started
- **#27** Per-version publish timestamps on component detail page — not started

---

## Shipped this session (2026-06-07)

- **#18** Proxy health indicator (green/red dot on home page)
- **#21** Proxy publish timestamps (CRAN `Published:`, Maven `lastUpdated`)
- **#22** Proxy browse populated: CRAN from PACKAGES file, Helm from index.yaml
- **#23** Dep links go to component page instead of search
- **Helm proxy**: added `helm-proxy` repo (Bitnami), fixed `parseIndexYAML` for both indent styles (Helm CLI indent-4 and Bitnami indent-2), wrapped description accumulation, `Inspect` uses `upstreamRecords` for proxy kind

---

## Key design decisions (locked)

- Auth: `RequireAdminUI` checks `Authorization: Bearer` header then `forge_token` HttpOnly SameSite=Strict cookie
- Session cookie name: `auth.UISessionCookie = "forge_token"`
- Open redirect prevention: `sanitizeNext()` rejects anything not starting with `/ui/`
- CSP: `script-src 'self' https://unpkg.com` — `theme.js` is served as `'self'`, no inline scripts
- Dark mode: `localStorage('forge-theme')` → `data-theme` on `<html>`; applied before first paint
- All assets embedded via `//go:embed templates static`
- No build step; stdlib + htmx 2.0.3 (unpkg, defer) only
- **BrowseRepo caching (#7):** Deliberately not solved — belongs with Phase 6 search/index service.
- **Helm proxy index.yaml parser:** line-scanner state machine, no YAML lib. Handles indent-2 (Bitnami) and indent-4 (Helm CLI) entry dash styles. Wrapped scalar descriptions accumulated via `contKey`/`contVal`. `helm-proxy` → `https://charts.bitnami.com/bitnami`.
