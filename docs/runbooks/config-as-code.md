# Config-as-Code runbook

forge can manage its own contents (repositories, cleanup policies, security
policies, roles, and webhooks) declaratively from a YAML or JSON file. Commit the
file to git, mount it as a Kubernetes ConfigMap, and forge converges on boot.

## File format: YAML or JSON

forge picks its parser from the file **extension** — `.yaml`/`.yml` parse as
YAML, anything else as JSON:

```bash
./forge -config forge.config.yaml     # YAML
./forge -config forge.config.json     # JSON
```

Both express exactly the same schema, and JSON is a subset of YAML 1.2, so
existing `.json` files keep working untouched — there is no migration step.

`${VAR}` placeholder expansion runs on the raw text *before* parsing, so secret
indirection behaves identically in both formats.

Export defaults to JSON (matching what the admin API emits, so **export → edit →
apply** round-trips without conversion); pass `-config-export-format=yaml` for
YAML.

## Unknown keys are an error

A key that matches no field is **rejected**, with its path and a suggestion:

```
config: forge.config.yaml: unknown field(s):
  "repositories[0].anonymusRead" (did you mean "anonymousRead"?)
  "totallyMadeUpTopLevelKey"
```

`encoding/json` would discard those silently. That was tolerable when config was
an additive seed loader; now that the file is the source of truth it is not — a
typo'd `prune` stops deletions happening, and a typo'd `claims` silently
disables dependency-confusion protection for a repository. Duplicate YAML keys
are rejected for the same reason (they otherwise keep the last value).

This strictness applies to **config files only**. Records read back out of the
meta store stay lenient about unknown fields, so a store written by a newer
forge is still readable by an older one.

### Gotcha: placeholders expand everywhere

`${VAR}` substitution runs on the raw text *before* parsing, which is what makes
it work identically for YAML and JSON. The consequence is that a placeholder is
expanded **anywhere** in the file, including inside comments — and an unset
variable is an error. Don't write the placeholder syntax in a comment unless
that variable is actually set.

## The config file schema

All sections are optional. A partial file is valid and additive (objects not
mentioned are left untouched unless `prune` is enabled).

```json
{
  "repositories":     [ ...repo.Repository objects...   ],
  "cleanupPolicies":  [ ...cleanup.NamedPolicy objects.. ],
  "securityPolicies": [ ...vuln.NamedPolicy objects...   ],
  "securityDefault":  { ...vuln.Policy...                },
  "roles":            [ ...auth.CustomRole objects...    ],
  "webhooks":         [ ...webhook.Subscription objects. ],
  "prune":            false,
  "adopt":            false
}
```

The JSON shape of each object matches exactly what the corresponding `GET`
endpoint returns — see `internal/repo/repo.go`, `internal/cleanup/policy.go`,
`internal/vuln/policy.go`, `internal/auth/roles.go`, `internal/webhook/webhook.go`.

### Key repository fields

| Field | Notes |
|-------|-------|
| `name` | Unique identifier. |
| `format` | `maven`, `npm`, `helm`, `cran`, `oci` |
| `kind` | `hosted`, `proxy`, `group` |
| `enabled` | Must be `true` for the repo to serve traffic (zero value = false). |
| `upstream` | Required for `proxy` repos. |
| `members` | Required for `group` repos — list of hosted/proxy repo names. |
| `cleanupPolicyName` | References a named cleanup policy (in file or store). |
| `securityPolicyName` | References a named security policy (in file or store). |
| `proxyAuth` | Upstream basic-auth credential. **Use `${ENV_VAR}`** — never commit. |

## Secret injection via ${ENV_VAR}

Any string value in the JSON can reference an environment variable:

```json
{
  "webhooks": [
    {"name": "ci", "url": "${WEBHOOK_URL}", "secret": "${WEBHOOK_SECRET}", "enabled": true}
  ]
}
```

forge expands `${VAR}` (and `$VAR`) placeholders from the process environment
before parsing. It is **a fatal error** to reference an undefined variable —
this prevents silently starting with blank secrets.

In Kubernetes, inject secrets via `extraEnvFrom` (Secret reference) rather than
`extraEnv` (plaintext in the values):

```yaml
extraEnvFrom:
  - secretRef:
      name: forge-webhook-secrets   # contains WEBHOOK_URL + WEBHOOK_SECRET
```

## CLI flags

| Flag | Behaviour |
|------|-----------|
| `-config <path>` | Read file, validate, apply on boot. Fatal if invalid. Skips hardcoded seed repos. |
| `-config-check` | Validate + print plan, exit 0 (valid) / 1 (invalid). No writes. For CI. |
| `-config-export` | Print current state as JSON to stdout, exit 0. Secrets are blanked. |
| `FORGE_CONFIG` env | Default value for `-config`; flag overrides. |

### -config-check in CI

Add this step after building the binary to catch config regressions per PR:

```yaml
- name: validate forge.config.yaml
  run: |
    go build -o forge ./cmd/forge
    WEBHOOK_URL=https://example.com WEBHOOK_SECRET=dummy \
      ./forge -config-check -config deploy/config/forge.example.yaml
```

## Prune semantics and the managed-set guarantee

By default, config reconcile is **additive**: it creates or updates the objects
in the file and never deletes anything.

Setting `"prune": true` enables deletion, but **only for objects that config
itself previously created**. forge tracks a managed-set in the meta store
(`admin:config-managed`). On each apply:

1. Objects in the file are created-or-updated.
2. Objects that are **in the managed-set but absent from the current file** are
   deleted.
3. Objects created via REST/UI are **never touched** — they are not in the
   managed-set unless config created them first.

This gives the GitOps guarantee without accidentally deleting repositories that
an operator added through the admin UI while the config file was being updated.

## Ownership: adoption vs update

The managed-set also decides what happens when the file names an object that
already exists. There are three cases:

| Object state | Disposition |
|---|---|
| Absent from the store | **Create** |
| Present, previously managed by config | **Update** (or noop if identical) |
| Present, **never** managed by config | **Adopt** |

Adoption is how an object created through the UI/API comes under config
ownership. It splits by whether the settings agree:

- **Identical** → adopted silently, no flag needed. This is the
  `-config-export` → commit → boot path, and it stays frictionless.
- **Different** → **refused**, naming the object and every differing field:

  ```
  config: refusing to adopt 1 object(s) this file has never managed and whose
  settings differ:
    repository "npm-proxy" differs in: upstream
  set "adopt": true (or pass -config-adopt) to take ownership and overwrite them
  ```

  Set `"adopt": true` in the file, or pass `-config-adopt`, to take ownership
  anyway. The existing settings are overwritten, the event is written to the
  audit log (`ADOPT`), and it is logged at WARN on boot.

This mirrors `kubectl apply --force-conflicts`: transferring ownership is never
the default, because a silent overwrite is how a config commit quietly reverts
somebody's console change. Once adopted, the object is config-managed — later
runs treat it as an ordinary update, and `prune` can delete it.

The refusal happens **before the first write**, so a rejected apply never leaves
state half-converged.

## Enforcement: the file wins

Under `-config`, objects the file manages are **read-only everywhere else**:

- The admin API returns **409** on any write to a config-managed repository,
  role, cleanup policy, security policy, or webhook, naming the object and the
  file that owns it.
- The UI badges them `config`, disables Configure/Edit/Delete, and renders the
  repository Settings tab inside a disabled `<fieldset>` with an explanatory
  banner. Controls are disabled **at the door** — a form that looks editable and
  then refuses to save is the most complained-about part of Grafana provisioning
  ([grafana/grafana#37679](https://github.com/grafana/grafana/issues/37679)).
- Every refusal is written to the audit log.

Objects created through the API/UI are untouched by this: ownership is per
object, from the managed set, not a global read-only mode.

`GET /api/v1/repos` (and the equivalent for each kind) reports
`"managedBy": "config" | "api"` so tooling can tell them apart.

### What is *not* gated

Config owns a repository's **definition**, not its **contents**. These stay
available on a config-managed repo, because they act on artifacts:

```
POST   /api/v1/repos/{name}/cleanup      run retention now
                                         (add ?dry=true to PREVIEW candidates;
                                          any other spelling deletes for real)
POST   /api/v1/repos/{name}/scan         vulnerability scan
POST   /api/v1/repos/{name}/promote      copy a component in
DELETE /api/v1/repos/{name}/component    delete one artifact
       .../invalidate .../reindex .../trash/...
```

### Break-glass

`-allow-config-override` permits writes to config-managed objects. Each one is
logged at WARN and audited, and the next apply reverts it. Use it for an
incident, not as a default.

There is deliberately **no self-heal** (silently reverting drift on a timer, as
Terraform and Argo CD `selfHeal` do). Refusing the write tells the operator
immediately; reverting it fifteen minutes later tells them after they think they
have fixed the outage.

## Drift: seeing when the file and reality disagree

`Apply` runs at boot, so between a commit and the next rollout the file and live
state can differ. Argo/Flux track the *ConfigMap*, not forge's objects, so that
window looked green.

```
GET /api/v1/config/drift        # admin-only; 404 outside -config mode
```

It re-reads the config file on every call — an updated ConfigMap is picked up
without a restart — and returns the same diff `Plan` computes:

```json
{
  "drift": true,
  "objects": 1,
  "source": "/etc/forge/forge.config.yaml",
  "kinds": { "repositories": { "created": 1, "updated": 0, "deleted": 0,
                               "adopted": 0, "noop": 2 }, ... },
  "conflicts": []
}
```

A config file that no longer parses or validates returns **500** rather than a
misleading "no drift".

The same numbers are exported for Prometheus, refreshed every
`-config-drift-interval` (default 1m; `0` disables the background check):

```
forge_config_drift_objects{kind="repositories",op="create"} 1
forge_config_drift_objects{kind="all",op="conflict"} 0
```

Alert on `sum(forge_config_drift_objects{op!="adopt"}) > 0` to catch an unapplied
commit or an override made during an incident.

## Continuous reconcile (opt-in)

By default forge converges **on boot only**: the `checksum/config` annotation
rolls the Deployment when the ConfigMap changes, and the new pod applies.

`-config-watch` re-applies when the file's *contents* change, so a commit takes
effect without a pod roll (mounted ConfigMaps are updated in place by the
kubelet, roughly once a minute). It polls at `-config-drift-interval`.

It triggers on the **file** changing, never on live state changing. Reverting an
operator's out-of-band edit on a timer is self-heal — Terraform and Argo CD
`selfHeal` do it, and the failure mode is someone fixing an outage at 03:00 and
watching it silently undone fifteen minutes later. forge refuses the write at
the door instead. A break-glass override therefore survives until someone
actually changes the config file.

A half-written or invalid file is reported and retried on the next tick, never
applied and never silently skipped.

## Reconcile dependency order

Apply runs in this order to satisfy references:

```
roles → cleanup policies → security policies (+default) → repositories → webhooks
```

Cross-references (a repo pointing at a cleanup or security policy) are validated
before any writes. If a policy name referenced by a repo is neither in the config
file nor in the store, the apply fails with a clear error — no partial state is
written.

## GitOps flow (Argo CD / Flux)

1. Author or export the config file (see [Migrating an existing deployment](#migrating)).
2. Commit the file to git (alongside the Helm values file).
3. Set `config.content` in your Helm values (inline) or point `existingConfigMap`
   at a pre-existing ConfigMap holding the content.
4. Argo CD / Flux sync renders the ConfigMap and the `checksum/config` annotation
   on the Deployment changes → Kubernetes rolls the pods → forge boots with the new
   config applied.

The ConfigMap **key is the filename**, and forge selects its parser from that
extension. The chart derives the `-config` path from the key, so the two can
never drift apart.

Recommended — `config.content` as a **map**, authored as native YAML. The chart
renders it to `forge.config.yaml`:

```yaml
config:
  content:
    repositories:
      - name: maven-central
        format: maven
        kind: proxy
        upstream: https://repo1.maven.org/maven2
    webhooks:
      - name: ci
        url: ${WEBHOOK_URL}
        secret: ${WEBHOOK_SECRET}
        enabled: true
extraEnvFrom:
  - secretRef:
      name: forge-webhook-secrets
```

Legacy — `config.content` as a **string** of inline JSON. Rendered to
`forge.config.json`; still supported unchanged:

```yaml
config:
  content: |
    {
      "repositories": [ ... ]
    }
```

Or with a separately managed ConfigMap:

```yaml
config:
  existingConfigMap: forge-config          # pre-existing CM
  existingConfigMapKey: forge.config.yaml  # default: forge.config.json
```

The `checksum/config` pod annotation is computed from the rendered content, so
editing the ConfigMap via `kubectl` and re-syncing the Deployment also triggers a
rollout.

## Migrating an existing (seeded) deployment

Export current state, review it, then switch to config mode:

```bash
# 1. Export current state (secrets are blanked). Add -config-export-format=yaml
#    for YAML instead.
./forge -config-export -data ./data > forge.config.json

# 2. Re-add secrets as ${ENV_VAR} placeholders manually, e.g.:
#    "proxyAuth": "${NEXUS_PROXY_CRED}"

# 3. Validate before committing.
MY_SECRET=x ./forge -config-check -config forge.config.json -data ./data

# 4. Commit the file and update Helm values to use config.content.
# From this boot onward the seed loop is skipped.
```

## Out of scope (v1)

These are documented as possible follow-ups; none are required for GitOps:

- **User and token reconcile** — human access is configured declaratively via
  OIDC group-mapping flags (`-oidc-group-mappings`); token rotation is inherently
  secret-lifecycle, not config-file territory.
- **Live file-watcher / SIGHUP reload** — a ConfigMap-checksum rollout is
  sufficient for GitOps; a watcher adds complexity without benefit at eval scale.
- **Terraform provider / Kubernetes CRDs** — possible future extensions; not
  required to make forge's configuration declarative and reproducible.
