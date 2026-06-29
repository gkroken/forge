# Config-as-Code runbook

forge can manage its own contents (repositories, cleanup policies, security
policies, roles, and webhooks) declaratively from a JSON file. Commit the file
to git, mount it as a Kubernetes ConfigMap, and forge converges on boot.

## Why JSON, not YAML

The Go standard library has no YAML parser. JSON is the same shape the admin
API emits (`GET /api/v1/repos`, etc.), so **export → edit → apply** round-trips
without conversion. YAML support would require an external dependency, which is a
non-goal.

## The forge.config.json schema

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
  "prune":            false
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
- name: validate forge.config.json
  run: |
    go build -o forge ./cmd/forge
    WEBHOOK_URL=https://example.com WEBHOOK_SECRET=dummy \
      ./forge -config-check -config deploy/config/forge.example.json
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

1. Author or export `forge.config.json` (see [Migrating an existing deployment](#migrating)).
2. Commit the file to git (alongside the Helm values file).
3. Set `config.content` in your Helm values (inline) or point `existingConfigMap`
   at a pre-existing ConfigMap whose `forge.config.json` key holds the content.
4. Argo CD / Flux sync renders the ConfigMap and the `checksum/config` annotation
   on the Deployment changes → Kubernetes rolls the pods → forge boots with the new
   config applied.

Example values override (inline content):

```yaml
config:
  content: |
    {
      "repositories": [ ... ],
      "webhooks": [{"name":"ci","url":"${WEBHOOK_URL}","secret":"${WEBHOOK_SECRET}","enabled":true}]
    }
extraEnvFrom:
  - secretRef:
      name: forge-webhook-secrets
```

Or with a separately managed ConfigMap:

```yaml
config:
  existingConfigMap: forge-config   # pre-existing CM; key must be forge.config.json
```

The `checksum/config` pod annotation is computed from the rendered content, so
editing the ConfigMap via `kubectl` and re-syncing the Deployment also triggers a
rollout.

## Migrating an existing (seeded) deployment

Export current state, review it, then switch to config mode:

```bash
# 1. Export current state (secrets are blanked).
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
- **YAML** — requires an external dependency, which is a non-goal.
- **Terraform provider / Kubernetes CRDs** — possible future extensions; not
  required to make forge's configuration declarative and reproducible.
