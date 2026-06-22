---
name: project-admin-auth-gotcha
description: Admin UI fetches must use a cookie-aware auth guard — the forge_token session cookie is HttpOnly so JS can't send it as a Bearer header.
metadata:
  type: project
---

The admin web UI authenticates with the **HttpOnly `forge_token` session cookie**
(set on login, `SameSite=Strict`). JS cannot read it, so UI `fetch()`/htmx calls
to `/api/v1/...` routes can only send it automatically as a cookie — never as an
`Authorization: Bearer` header.

**Why it matters:** `auth.Enforcer` has two admin guards (`internal/auth/enforce.go`):
- `RequireAdmin` — reads Bearer **and now the cookie**; returns 401 on failure (for XHR/API).
- `RequireAdminUI` — reads Bearer or cookie; **redirects** (303) to login on failure (for page loads).

**How to apply:** Any `/api/v1` endpoint that the admin UI calls from the browser
must use a cookie-aware guard. Originally only `RequireAdminUI` read the cookie, so
the repo-config **Access** and **Activity** tabs (+ cache-stats/health, role delete)
showed "Failed to load…" — their Bearer-only `RequireAdmin` returned 401 to the
cookie-only fetch. Fixed `2026-06-22` (`7d4e2da`) by making `RequireAdmin` fall back
to the cookie too (CSRF-safe via SameSite=Strict). Regression test:
`TestRequireAdmin_AcceptsSessionCookie`. Don't guard a browser-called JSON endpoint
with a Bearer-only check.
