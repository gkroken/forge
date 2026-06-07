---
name: project-ui-workplan
description: UI workplan status — U0/U1 done, U2 partially done (component detail stub + #12–#18 open), U3 mostly done (#19/#20 open). Source of truth: WORKPLAN-UI.md.
metadata:
  type: project
---

UI workplan lives in `WORKPLAN-UI.md`. WORKPLAN.md is prototype-era and NOT authoritative for UI work.

**Why:** Accurate status is needed to plan next UI work. Previous memory incorrectly marked all phases complete.

**How to apply:** Check the status column in the gap table before assuming a feature is done. Component detail enrichment and the two new polish items are the open work.

---

## Phase summary (verified against codebase 2026-06-07)

| Phase | Status | Notes |
|-------|--------|-------|
| U0 | ✅ COMPLETE | Auth guard on all `/ui/admin/*` mutations; login/logout; `RequireAdminUI` cookie+header check |
| U1 | ✅ COMPLETE | Full handler+fragment test suite; ≥80% coverage on UI handlers |
| U2 | ⚠️ PARTIAL | Token mgmt, search filters, upload, access view done. Component detail is a stub; #12–#18 open. |
| U3 | ⚠️ PARTIAL | Cache-busting, dark mode, nav hx-boost, admin breadcrumb done. #19 (sortable columns) and #20 (format icons) open. |

---

## Open gaps (❌)

All sourced from `WORKPLAN-UI.md` §4 gap table.

**Component detail page enrichment (U2):**
- #12 Format-specific install snippets
- #13 Package description & license
- #14 README/long description (plain text)
- #15 Dependency list (flat, with internal links)
- #16 Per-version direct download links
- #17 Last-published timestamp (`UpdatedAt` missing from `BrowseEntry` struct and all format handlers)
- #18 Proxy health indicator (green/amber dot from existing cache metadata)

**Polish (U3):**
- #19 Sortable columns in listings (htmx or minimal inline script; no JS lib)
- #20 Format/language icons on badges (inline SVG or static files; must fit existing CSP) ✅ done
- #21 Proxy packages show no last-published timestamp — fix by forwarding upstream publish time (npm: `time.modified` from packument; CRAN: `Date/Publication` from PACKAGES index; Helm: `created` from index.yaml; Maven: `lastUpdated` from metadata; OCI: skip or use manifest `created` annotation)

---

## Key design decisions (locked)

- Auth: `RequireAdminUI` checks `Authorization: Bearer` header then `forge_token` HttpOnly SameSite=Strict cookie; redirects to `/ui/login?next=…` on failure; eval mode (nil Store) passes through
- Session cookie name: `auth.UISessionCookie = "forge_token"`
- Open redirect prevention: `sanitizeNext()` rejects anything not starting with `/ui/`
- CSP: `script-src 'self' https://unpkg.com` — `theme.js` is served as `'self'`, no inline scripts
- Dark mode: `localStorage('forge-theme')` → `data-theme` attribute on `<html>`; `theme.js` applies it before first paint
- All assets embedded via `//go:embed templates static`
- No build step; stdlib + htmx 2.0.3 (unpkg, defer) only

**BrowseRepo caching (#7):** Deliberately not solved — belongs with the Phase 6 search/index service, not the UI layer.
