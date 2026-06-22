---
name: project-design-sweep
description: Per-page design-signature sweep across the Foundry UI (mono "instrument-panel" voice). Wave 1 done; Wave 2 in progress as of 2026-06-21.
metadata:
  type: project
---

Frontend-design signature sweep on branch `feature/foundry-remaining-tabs`,
applying one cohesive visual voice page-by-page. Distinct from the build-phase
work in [[project-ui-workplan]] — this is pure design polish on shipped pages.

**The signature ("instrument-panel" voice):** structural labels in IBM Plex Mono,
uppercase, tracked (the eyebrow voice — `.inst-label`, `.browse-coord-label`,
`.rc-legend`, `.rail-card-title`, `thead th`); values in tabular-mono; metric
tiles render as the hairline-divided **instrument readout strip**
(`.instrument-panel` → `.inst-readouts` → `.inst-readout`). Form *field* labels
stay sans (readability). Boldness spent in one place per screen; rest stays quiet.
Foundation (tokens + mono eyebrows + tabular numerals) shipped in `93ede44`.

**Wave 1 — flagship (✅ complete):**
- ① Dashboard → instrument-panel status readout (`49f4dd3`): mono strip
  `FORGE · EVAL MODE · UP hh:mm:ss · ● OPERATIONAL` over 4 gauge tiles. Status
  derived honestly from service-health rows; uptime from `Server.started`.
- ② Browse → "coordinate readout" (`1ee0f15`): center breadcrumb recast as mono
  Location address (origin repo / path / component); detail pane = mono spec-sheet.
  Fixed broken ⧉ copy glyph → `content_copy` Material Symbol.
- ③ Repo config tabs (`5a1d19b`): fieldset legends + rail titles → mono voice via
  new `.rc-fieldset`/`.rc-legend` (lifted inline-style debt). Field labels stay sans.

**Wave 2 — supporting:**
- Repos list ✅ (`544b6f5`): health dots onto muted Foundry tokens (`--dot-ok/err`,
  was raw `#22c55e/#ef4444`); headers to canonical eyebrow tracking.
- Observability ✅ (`65e0a16`): KPI row → instrument readout strip; status legend → mono.
- Cleanup ✅ (`33adc24`): last `.kpi-card` user retired → readout strip. Added
  `.inst-value-text` (20px) for pre-formatted string values (bytes/dates) to avoid overflow.
- Tokens & Access ✅ (`47b94ff`): `.section-title` (last sans-bold structural label) →
  mono eyebrow voice; also unifies Observability/Cleanup section dividers (shared class).
  Must run app with `-auth` to see this page. While here, fixed a real clipping bug
  (`f66a83b`): `.admin-content` flex children defaulted to `flex-shrink:1`, so once content
  overflowed the viewport a tall `.admin-table-wrap` (overflow:hidden) shrank and clipped
  its own rows → pinned `.admin-content > * { flex-shrink: 0 }`.
- Search ✅ (`95f52da`): plain `.search-meta` count line → mono tabular readout
  ("RESULTS 89 for <q>"), echoing the Browse coordinate readout. Table headers +
  LATEST were already on-voice. Filter inputs stay sans (form controls).
- Upload ✅ (`7664d59`): dropped redundant `<h1>` (topbar already titles it), recast
  repo-meta into an `.upload-dest` destination readout ("UPLOADING TO <repo> [fmt][kind]").
  Scoped to upload-only class — shared `.breadcrumb`/`.repo-meta` (component + cleanup
  forms) untouched. Verified helm/cran forms + maven CLI-hint variant. Must run with `-auth`.
- Component detail ✅ (`1c03bfd`): dropped redundant sans `<h1>` name, recast `.page-header`
  into `.comp-ident` identity readout ("COMPONENT <name>" with name as mono coordinate +
  badges). Scoped to component-only class — shared `.page-header` (access.html) untouched.
  Section h2s / version table / install snippet were already on-voice.

**Wave 2 COMPLETE.** All supporting pages done. Established readout pattern now spans
Browse (coordinate), Search (results), Upload (destination), Component (identity) —
all: mono eyebrow + emphasized mono/tabular value, scoped to per-page classes registered
in the shared eyebrow group (style.css ~line 911).

**Wave 3 DONE.**
- Dead CSS sweep (`694432f`): removed the entire `.kpi-*`/`.stat-*` card block + their
  selectors in the shared eyebrow/tabular groups. All were orphaned (every page moved to
  `.inst-*` readout strip; rail uses separate `.rail-stat-*`). Dashboard verified unchanged.
- Accessory trim (`4235d45`): removed the non-functional "Filter tokens…/users…" fake
  search boxes on the Tokens/Users tabs (styled div, no input/JS) → kept the live count.
- Critique outcome: other pages reviewed at 2× and left as-is — already clean; forcing a
  removal-per-page would degrade good pages. Search filters are real (not trimmed).

**ENTIRE FOUNDRY DESIGN SWEEP COMPLETE** (Waves 1–3). Readout signature spans all pages;
voice is consistent; dead CSS gone. Also fixed an unrelated auth bug found mid-sweep —
see [[project-admin-auth-gotcha]] (`7d4e2da`).

**Wave 3:** final 2× critique pass; trim one accessory per page. Also sweep the now-dead
`.kpi-card`/`.kpi-grid` CSS (all three users migrated to the readout strip).

**Workflow notes:** verify every page live at 2× before committing. Local server:
`./forge -addr :8080 -data ./data` (background). Screenshots via the playwright
chromium at `~/.cache/ms-playwright/chromium-1223/chrome-linux64/chrome` headless
`--force-device-scale-factor=2 --screenshot=`. Commit per page, stage only sweep files.
