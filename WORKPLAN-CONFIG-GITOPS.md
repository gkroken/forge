# WORKPLAN — Config-as-Code v2 (GitOps source of truth)

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

Config-as-code v1 shipped (`internal/config/config.go`: `Load`/`validate`/`Plan`/
`Apply`/`Export`, `${ENV}` secret indirection, managed-set prune, `-config`,
`-config-check`, `-config-export`, Helm `config.content` + `checksum/config`
rollout). It is a **boot-time importer**, not a source of truth. This plan closes
that gap.

**Acceptance for the whole track:** a committed YAML file is the authoritative
definition of repos, roles/grants, cleanup policies, security policies and
webhooks; config-owned objects are visibly marked and refuse UI/API writes;
adoption of pre-existing objects is conflict-checked, never silent; drift is
observable. `go test ./...` (incl. `-race`), `go vet`, `bash test.sh` green.

## Gap → phase matrix

| # | Gap | Phase | Status |
|---|-----|-------|--------|
| 1 | Config is JSON only; GitOps users expect YAML | C1 | ✅ |
| 2 | `-config-export` emits JSON only | C1 | ✅ |
| 3 | Helm `config.content` is a raw JSON string, not structured YAML | C1 | ✅ |
| 4 | `Apply` silently overwrites + adopts UI-created objects (`config.go:427`) | C2 | ✅ |
| 5 | `Plan` cannot distinguish "mine" from "someone else's" | C2 | ✅ |
| 6 | No conflict check on adoption (Grafana footgun, not K8s SSA) | C2 | ✅ |
| 7 | Config-owned objects are freely editable via UI/API | C3 | ✅ |
| 8 | Object ownership is invisible in API responses and UI | C3 | ✅ |
| 9 | No break-glass path for a 03:00 incident | C3 | ✅ |
| 10 | Drift between file and state is silent until next boot | C4 | ✅ |
| 11 | No drift metric → Argo/Flux cannot show OutOfSync for forge state | C4 | ✅ |

## Prior art (researched 2026-08-25 — informs C2/C3)

| System | On name collision | Lesson taken |
|---|---|---|
| Terraform | Refuses; manual `terraform import` ([#29160](https://github.com/hashicorp/terraform/issues/29160) open for years) | Outlier; most-complained-about. Do **not** refuse outright |
| Grafana | Adopts and overwrites silently; docs warn against colliding titles/UIDs | This is forge's current behaviour. It is the footgun |
| Kubernetes SSA | Adopts, but conflicting **fields** error unless `--force-conflicts`, which transfers ownership. Docs: don't default to force; surface conflicts | The model to copy |
| Crossplane | Staged: `managementPolicies: ["Observe"]` imports without modifying, then graduate | `-config-check` is already forge's Observe mode |

Also [grafana/grafana#37679](https://github.com/grafana/grafana/issues/37679): with
`allowUiUpdates: false` the UI still renders an editable form that fails on save.
**Block at the door — disable controls and badge the object; never 409 a submit.**

---

## Phase C1 — YAML as the authored format

**STATUS: C1 COMPLETE.** Dependency approved 2026-08-25. Measured cost was
**one new module** — `sigs.k8s.io/yaml v1.6.0` with **zero** new transitive
modules, because its YAML backends (`go.yaml.in/yaml/v2`, `v3`) were already
indirect dependencies via testcontainers/minio. `go test ./...`, `go vet`, and
`bash test.sh` (20/20) green.

**⚠️ ORIGINAL DECISION NOTE.** CLAUDE.md mandates Go stdlib only. This
phase adds **`sigs.k8s.io/yaml`**. Chosen over
`gopkg.in/yaml.v3` directly because it converts YAML→JSON and delegates to
`encoding/json`, so **every existing struct tag works unchanged** — no `yaml:` tags
across `repo`, `cleanup`, `vuln`, `auth`, `webhook`. Vendor it. Do not start C1
without sign-off.

*Zero-dependency fallback if declined:* structured `config.content` in `values.yaml`
+ `toJson` in the ConfigMap template (gap #3 only). YAML then works **only** inside
Helm values — not for a committed `forge.config.yaml`, `existingConfigMap`, local
dev, or export. Gaps #1/#2 stay open.

Acceptance: `./forge -config forge.config.yaml` works; existing JSON files still
parse byte-identically (JSON ⊂ YAML 1.2 — no migration, no dual code path).

- [x] **#1 YAML load.** `Load()` switches on file extension (`.yaml`/`.yml` → `sigs.k8s.io/yaml`,
      else `encoding/json`). `${ENV}` expansion runs on the raw text **before** parsing, so
      secret indirection is format-agnostic and unchanged.
- [x] **#2 YAML export.** `-config-export-format=json|yaml` (default `json` for compat).
      Invalid values exit 1 with a clear message.
- [x] **#3 Helm structured content.** `config-configmap.yaml` accepts a map or a string:
      `{{- if kindIs "string" .Values.config.content }}` … `{{- else }}{{ toJson }}{{- end }}`.
      `deployment.yaml:33` checksum needs `toJson | sha256sum` on the non-string branch.
      Existing string configs keep working. **Implemented differently than planned:** the
      ConfigMap key is now the filename and a `forge.configFileName` helper derives BOTH the
      key and the `-config` path from it, so map form renders native YAML to
      `forge.config.yaml` (not JSON via `toJson`) and the two can never drift apart. Also
      added `config.existingConfigMapKey`.
- [x] **Bug found + fixed (pre-existing).** The chart mounted the ConfigMap at `/etc/forge`
      with key `forge.config.json` — so the file landed at `/etc/forge/forge.config.json` —
      but passed `-config /etc/forge/config.json`. Config-as-code via the Helm chart would
      crashloop at boot. Fixed by the `forge.configFileName` helper above.
- [x] **Bug found + fixed (pre-existing).** `values.schema.json` pinned `config.content` to
      `"string"`, and the `content: ""` default made Helm's coalesce silently discard a
      user-supplied map ("destination for forge.config.content is a table"). Schema now
      accepts string|object|null and the default is `null`.
- [x] Tests: YAML/JSON round-trip equivalence on the same logical config; `${ENV}` expansion
      in YAML incl. unset-var error; `kindIs` both branches via `helm template`.
- [x] Docs: `docs/runbooks/config-as-code.md` — YAML examples, remove the "YAML requires an
      external dependency, which is a non-goal" line.

## Phase C2 — Ownership: adopt ≠ update

**STATUS: C2 COMPLETE.** `classify()` is the single decision point shared by
Plan and Apply, so the two can no longer disagree about an object. Apply now
runs Plan first and refuses *before the first write*, keeping a rejected apply
from leaving state half-converged. 7 new unit tests + live-verified against the
13 seeded repos: export→check reports `repos_adopt=13, conflicts=0` (exit 0);
one altered upstream reports `repository "npm-proxy" differs in: upstream`
(exit 1); the same file with `-config-adopt` passes (exit 0) with a warning.
`go test ./...` (incl. `-race`), `go vet`, `bash test.sh` (20/20) green.

Acceptance: `Plan` reports **Adopted** separately from Created/Updated/Noop/Deleted;
adopting an object whose fields differ is refused by default with the conflicting
field names listed; `-config-export` → commit → boot adopts cleanly with zero prompts.

- [x] **#5 Managed-set cross-reference.** `Plan`/`Apply` classify against `loadManaged()`,
      not just store presence: in store **and** in managed set → Update; in store, **not** in
      managed set → **Adopt**. Applies to all five kinds (repos, roles, cleanup, security,
      webhooks). Add `Adopted int` to `KindResult`; include in `Changes()`.
- [x] **#6 Conflict check.** Adopt with `jsonEqual(desired, existing)` → free, silent
      (this is the export→commit path; it must stay frictionless). Adopt with differing
      fields → **refuse**, error naming object + differing field names.
- [x] **#4 Explicit override.** `adopt: true` in `File` (and `-config-adopt` flag) permits
      conflicting adoption; transfers ownership into the managed set. Per K8s guidance this
      is **never** the default. Every forced adoption writes an audit entry.
- [x] `-config-check` output gains an Adopted line + the conflict list — this is the
      Crossplane observe-first stage for a Nexus migration.
- [x] Tests: adopt-identical is a no-op; adopt-conflicting refuses and names fields;
      `adopt: true` succeeds + audits + object lands in managed set; adopted object is
      subsequently prunable; unchanged prune semantics for API-created objects.

## Phase C3 — Enforcement (the file actually wins)

**STATUS: C3 COMPLETE.** 13 new tests; live-verified (409 with source+remedy,
`managedBy` in listings, badge + disabled fieldset in the UI, audited refusal).
`go test ./...`, `go vet`, `bash test.sh` (20/20) green.

**Two corrections to the plan below, both found while implementing:**

1. **Not middleware over the route registrations.** `/api/v1/repos/{name}/`
   carries ~14 sub-resources (`cleanup`, `scan`, `promote`, `component`,
   `trash`, `invalidate`, `reindex`, …) that mutate a repo's **artifacts**, not
   its definition. A blanket route gate would have broken every one of them on
   any config-managed repo. The gate is applied at the specific mutation points
   instead; `TestConfigOwned_ContentRoutesNotGated` guards the distinction.
2. **The UI was a second bypass.** `uiAdminDeleteRepo`, `processRepoForm`, and
   the cleanup-policy repo assignment call `s.Repos.Update/Delete` directly, so
   the API gate did not cover clicking. Those paths are gated too, with tests.

Acceptance: a config-owned repo cannot be edited or deleted through UI or API; the
UI shows it as managed with the source path; break-glass exists and leaves a trace.

- [x] **#8 Surface ownership.** `managedBy: "config" | "api"` on objects returned by
      `/api/v1/{repos,roles,cleanup-policies,security-policies,webhooks}`, derived from the
      managed set.
- [x] **#7 Gate mutating writes.** Middleware over the 10 route registrations at
      `server.go:379-394`: non-GET to a config-owned object → **409** with the config source
      path. Read paths untouched.
- [x] **#7 UI: block at the door.** "Managed by config" badge + **disabled** edit/delete
      controls on `admin_repos.html`, `admin_repo_form.html`, `cleanup_policies.html`,
      `security_policies.html`, `webhooks.html`, `access.html`. Follow existing CSP-safe
      conventions — **zero inline handlers**. Explicitly avoids grafana#37679.
- [x] **#9 Break-glass.** `-allow-config-override` starts forge with the gate off (409s
      become warnings). Every override write audits with actor + object, and marks the object
      drifted until the next `Apply`. No per-request force header — a flag is greppable in a
      postmortem; a header is not.
- [x] Tests: 409 on write to config-owned repo/role/policy/webhook; 200 on API-created ones;
      GET unaffected; override flag permits + audits; template render asserts disabled controls.

## Phase C4 — Drift visibility

**STATUS: C4 COMPLETE — the track is done.** 11 new tests; live-verified
(drift flips true on a file edit with no restart, the gauge appears in a real
`/metrics` scrape, and `-config-watch` applies an appended repo within one
tick). `go test ./...` (incl. `-race`), `go vet`, `bash test.sh` (20/20) green.

Note on the watcher: its first tick applies unconditionally, so a change landing
between the boot apply and the watcher starting is never missed. After that it
triggers on file-content changes only — `TestWatch_IgnoresLiveStateChanges`
pins the no-self-heal decision.

Acceptance: drift between the on-disk file and live state is queryable and
scrapeable; Argo/Flux can show forge state OutOfSync, not just the ConfigMap.

- [x] **#10 Drift endpoint.** `GET /api/v1/config/drift` → re-runs `Plan()` against the
      `-config` file and returns the `Result` (incl. Adopted + per-object detail). Read-only;
      admin-scoped. 404 when not in `-config` mode.
- [x] **#11 Drift metric.** `forge_config_drift_objects{kind}` gauge, refreshed on the same
      tick as the drift computation.
- [x] **Optional — file-watch reconcile.** DONE, opt-in via `-config-watch`. Poll the config file mtime/hash (~60s; matches
      kubelet ConfigMap sync) and re-`Apply` on change, so a commit takes effect without a pod
      roll. Subsumes the periodic drift computation. **Cut this if C1–C3 slip** — the Argo
      `checksum/config` rollout already closes the loop, just with a restart.
- [x] Tests: drift endpoint reports a UI-created divergence; returns clean after `Apply`;
      404 outside `-config` mode; gauge tracks the endpoint; watcher re-applies on change.

---

## Out of scope (v2)

- **User and token reconcile.** Roles + grants are declarative; *membership* comes from the
  IdP. LDAP group mappings can already live in the config `ldap` block; OIDC group mappings
  remain `-oidc-group-mappings` flags. Local-user membership stays imperative. Token rotation
  is secret lifecycle, not config-file territory.
- **Forge pulling git directly.** Argo/Flux/Helm own the git→cluster leg. Forge reading a
  remote repo means a git client (no stdlib git) and credential handling for marginal gain.
- **Kubernetes CRD / operator, Terraform provider.** Both sit on top of C2+C3 and only make
  sense once ownership and enforcement are real. Revisit after this track.
- **Blob stores in config.** `/api/v1/blob-stores` is GET-only; backends are process config
  (flags/env), correctly Helm's territory.
- **Self-heal (revert drift after the fact).** Deliberately rejected. Blocking the write at
  the door tells the operator immediately; reverting tells them fifteen minutes later, when
  they think they've fixed something. Terraform/Argo `selfHeal` failure mode.
