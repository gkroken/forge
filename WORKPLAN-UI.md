# forge Web UI — Workplan

Scope: the server-rendered web UI under `internal/server` (pages, htmx fragments,
admin, and the JSON APIs the UI consumes). This is the **source of truth for UI
work**. The root `WORKPLAN.md` is prototype-era and out of date; do not treat it
as authoritative for the UI. The principles below (test-first, stdlib-only,
exit-criteria-gated) are carried over because they still hold, not because that
document is current.

---

## 0. Current state (working)

- Server-rendered Go `html/template`, buffered before write; htmx 2.0.3 (unpkg,
  `defer`) used for exactly two live-swap surfaces: repo-detail filter and global
  search. Hand-rolled CSS, no framework, no build step. All templates + static
  assets embedded via `//go:embed`.
- **Pages:** home (repo list), repo detail (filter + components), component
  detail *(stub — versions + registry URL only; enrichment pending)*,
  search (with format/repo filters), admin repo list/create/edit, token
  management, access/visibility view, upload (npm/helm/cran), login/logout.
- **JSON APIs the UI can call:** repos CRUD, `repos/{name}/components`,
  `search`, `tokens` (create/list/revoke).
- **Browse:** all five formats (maven/npm/helm/cran/oci) implement
  `format.Browsable`; filtering/pagination happen in the server handlers.
- **Auth:** `RequireAdminUI` checks `Authorization: Bearer` then `forge_token`
  HttpOnly cookie; redirects to `/ui/login?next=…` on failure; eval mode
  passes through. All UI mutations are guarded.
- **Open work:** component detail enrichment (#12–#16), last-published
  timestamp (#17), proxy health indicator (#18), sortable columns (#19),
  format/language icons (#20).

---

## 1. Definition of Done (UI)

GA for the UI is reached when **all** hold:

- No UI route can read or mutate anything without going through the same authz
  decision the JSON/`/repository/` routes already enforce.
- Every UI route **and every htmx fragment** has a handler test; `HX-Request`
  paths are asserted to swap only their named block.
- Security headers asserted on every route (already covered by
  `security_headers_test.go`; new routes must keep it green).
- Each user-facing surface (browse, search, token mgmt, upload, admin) is usable
  end-to-end; the upload surface is covered by a click-through that drives a real
  publish and verifies the artifact then appears in browse.
- Runs with stdlib + htmx only, no build step, all assets embedded. CSP stays
  `script-src 'self' https://unpkg.com`.

---

## 2. Guiding principles

1. **Test-first for new surfaces.** Handler/fragment tests land with (or before)
   the feature, not after.
2. **One authz path.** The UI does not get its own weaker check; it reuses the
   Enforcer.
3. **No new runtime deps, no build step.** stdlib + the already-vendored htmx.
   Any additional JS needs explicit approval against the CSP.
4. **Assets stay embedded.** Everything under `internal/server/templates/` or
   `internal/server/static/`.

---

## 3. Phases

### Phase U0 — Auth guard on UI writes  *(blocker)*
The admin form POST/DELETE handlers call `s.Repos.Add/Update/Delete` directly,
bypassing the Enforcer. Close this before anything else.

- Route `/ui/admin/*` mutations (POST `new`, POST `{name}/edit`,
  DELETE `{name}`) through the same authz path as `/api/v1/repos`.
- **Open design fork (decide here):** the Enforcer is `Authorization`-header
  oriented and the UI has no session concept. Either (a) add a browser
  session/cookie login that carries an admin token, or (b) require the token
  mechanism for UI writes (login form stores it). Read paths may stay open or
  move behind anonymous-read policy — decide alongside.

**Exit:** unauthenticated UI POST/DELETE → 403; authz tests cover the UI admin
routes; `security_headers_test` green.

### Phase U1 — UI test harness  *(before U2 features)*
- Extend `ui_test.go`: table-driven render test per page; per-fragment test
  asserting `HX-Request: true` returns only the swapped block
  (`components-section`, `search-results`).
- Auth-enforcement assertions for U0.
- Form validate/re-render test (error preserves user input; success redirects
  with flash).
- Optional: golden HTML for the stable templates (`-update` convention).

**Exit:** every UI route + fragment has a test; `internal/server` coverage at the
agreed bar (propose ≥80% for the UI handlers).

### Phase U2 — Functional gaps  *(parallelizable; writes gated on U0)*
- **Token management UI** (#3) ✅ `tokens.html` + `tokens.go`
- **Search filters** (#5) ✅ `?format` / `?repo` controls in `search.html`
- **Upload UI** (#10) ✅ `upload.html` + `ui_upload.go`; publish→browse click-through passes
- **Access/visibility view** (#4) ✅ `access.html`
- **Per-component detail page** (#2) ⚠️ *stub* — route and template exist but
  show only versions list + registry URL copy. Enrichment items below are **not
  yet implemented**:
  - **Format-specific install snippets** (#12): exact copy-pasteable install
    command per format (npm install, install.packages, helm repo add + install,
    Maven pom.xml stanza) using the forge registry URL, pre-filled per component.
  - **Package description & license** (#13): surface the human-readable
    description and license from stored metadata (npm packument `description`,
    CRAN DESCRIPTION `Description:`/`License:`, Helm chart.yaml `description`,
    Maven POM `<description>`).
  - **README / long description rendering** (#14): npm packuments carry a
    `readme` field; CRAN has `Description:`. Render as plain text (not markdown,
    no new dep) on the component page.
  - **Dependency list** (#15): show direct deps from stored metadata — npm
    `dependencies`, CRAN `Depends`/`Imports`, Helm chart dependencies, Maven
    POM `<dependencies>`. No graph, just a flat list with links to those
    components within forge where they exist.
  - **Per-version direct download links** (#16): a download link per version
    artifact rather than just the registry base URL.
- **Last published timestamp** (#17): add `UpdatedAt` to `BrowseEntry` (set at
  write time), surface as a column in the repo component table. `BrowseEntry`
  currently has only `Name` and `Versions` — field and write-path wiring needed
  across all format handlers.
- **Proxy health indicator** (#18): small green/amber status dot on the repo
  list showing whether the upstream was reachable on the last proxy fetch.
  Source from the existing proxy cache metadata (no new polling).

**Exit:** #12–#18 shipped with handler tests; component detail page fully
populated for at least npm and CRAN.

### Phase U3 — Polish & operability
- **Static asset cache-busting** (#11) ✅ `style.css?v={{cssVer}}` in `base.html`
- **Dark mode** (#1) ✅ `theme.js` + `localStorage` toggle; applies before first paint
- **Nav-search → htmx consistency** (#8) ✅ `hx-boost="true"` on nav form
- **Admin breadcrumb** (#9) ✅ present in `admin_repos.html`
- **`BrowseRepo` caching** (#7): triaged — belongs with Phase 6 search/index
  service, not the UI layer. Not solved here.
- **Sortable columns** (#19): clicking a column header in any listing (repo
  component table, search results, admin repo list) sorts by that column
  client-side for the current page, or passes a `?sort=` param to the server
  for cross-page sort. No JS library — pure htmx or minimal inline script.
- **Format/language icons** (#20) ✅ Official Simple Icons (CC0) inlined via
  `formatIcon` template func in `ui.go`; `fill="currentColor"` inherits badge
  text colour in both themes. Applied to all five format badges across home,
  repo, component, search, and admin templates.

**Exit:** #19 and #20 shipped; existing handler tests stay green; security
headers test green on any new routes.

---

## 4. Known-gap → phase map

| # | Gap | Phase | Status |
|---|-----|-------|--------|
| 6 | Admin has no auth guard | U0 | ✅ done |
| — | UI test coverage of routes/fragments | U1 | ✅ done |
| 3 | No token management UI | U2 | ✅ done |
| 5 | Search filters not exposed | U2 | ✅ done |
| 10 | No upload UI | U2 | ✅ done |
| 4 | No user/access visibility | U2 | ✅ done |
| 2 | Component detail page is a stub | U2 | ⚠️ stub |
| 12 | No format-specific install snippets | U2 (component page) | ❌ open |
| 13 | Package description & license not surfaced | U2 (component page) | ❌ open |
| 14 | README/long description not rendered | U2 (component page) | ❌ open |
| 15 | Dependency list not shown | U2 (component page) | ❌ open |
| 16 | No per-version direct download links | U2 (component page) | ❌ open |
| 17 | No last-published timestamp in listing | U2 (repo page) | ❌ open |
| 18 | No proxy upstream health indicator | U2 (repo list) | ❌ open |
| 11 | Static files not cache-busted | U3 | ✅ done |
| 1 | No dark mode | U3 | ✅ done |
| 8 | Nav search not htmx (inconsistent) | U3 | ✅ done |
| 9 | No breadcrumb on admin home | U3 | ✅ done |
| 7 | `BrowseRepo` full-load, no caching | U3 (triage → perf/index) | ✅ triaged |
| 19 | Columns in listings are not sortable | U3 | ❌ open |
| 20 | No format/language icons on badges | U3 | ✅ done |

---

## 5. Tests

- **Handler/fragment tests** (`ui_test.go`): render every page and every
  htmx-swappable block; assert the fragment path returns only its block.
- **Auth tests:** every mutating UI route denies without a policy decision.
- **Security headers** (`security_headers_test.go`): already asserts required
  headers on every route — new routes must not regress it.
- **Upload click-through:** publish through the UI, then assert the artifact is
  served and shows in browse (the one E2E-ish UI scenario).

---

## 6. Constraints (carried through every phase)

- Go stdlib only; no new Go dependency without explicit approval.
- No build step; CSS/JS usable as-is.
- Assets embedded under `internal/server/templates/` or `.../static/`.
- htmx only for client interactivity; additional JS must justify itself against
  CSP `script-src 'self' https://unpkg.com`.
- `go test ./...` stays green.