# Migrating from Sonatype Nexus 3

forge imports repositories, content and permissions from a **live Nexus 3
server** over its REST API. Nothing on the source is ever modified, and
nothing is written to forge until you apply a reviewed plan.

Reading an on-disk Nexus blobstore is deliberately out of scope: the
blobstore layout is internal to Nexus (its metadata database is required to
interpret it), while the REST API works against any deployment — including
S3-backed ones — and reports per-asset checksums that double as transfer
verification.

## What you need

- A Nexus 3 URL reachable from the forge server, plus credentials.
  **Admin credentials are strongly recommended**: repository settings (proxy
  upstream URLs, group members) and the security listings need them. With
  weaker credentials the plan degrades honestly — proxy repos without a
  readable remote URL are skipped with a reason.
- The async worker (any non-eval deployment has it; eval mode does too).
- Global admin on forge (the migration API is `RequireAdmin`).

## Flow

1. **Plan** — `POST /api/v1/migration/plan` or the **Migration** page
   (`/ui/admin/migration`):

   ```bash
   curl -H "Authorization: Bearer $FORGE_TOKEN" \
     -X POST -H 'Content-Type: application/json' \
     -d '{"url":"http://nexus:8081","username":"admin","password":"…","includeSecurity":true}' \
     http://forge:8080/api/v1/migration/plan
   ```

   The plan is a persisted dry-run: every source repo with a
   `create` / `exists` (reuse) / `skip` decision and the reason, component +
   asset counts for hosted repos, and — with `includeSecurity` — the grant
   translation per role and the fate of every user. An optional `"repos":
   ["a","b"]` filter migrates a subset.

2. **Review.** Skips are stated plainly: unsupported formats (anything
   outside maven2/npm/helm/r/docker), name conflicts with an existing forge
   repo of a different shape, proxies with unreadable settings, privileges
   with no forge equivalent, content selectors beyond the translatable
   subset.

3. **Apply** — `POST /api/v1/migration/apply` (202). One `migration.run` job
   executes on the shared worker; `GET /api/v1/migration` (or the page)
   shows live per-repo progress. Hosted and proxy repos migrate before
   groups so members exist first.

4. **Acceptance loop.** After each hosted repo the job records
   source-vs-target counts and enqueues a **FULL integrity verify** — the
   same read-only checker used for restore validation. A repo is done when
   its counts match and its verify report says intact.

## What migrates, and how

| Nexus | forge | Content |
|---|---|---|
| maven2 hosted | maven hosted | every asset at the same path, **including checksum sidecars** (the post-migration verify re-hashes each artifact against them — end-to-end transfer verification). `maven-metadata.xml` is skipped: forge generates it. |
| npm hosted | npm hosted | per-version publishes rebuilt from the source packument; dist-tags reconstructed exactly; tarball URLs re-baked to forge's host |
| helm hosted | helm hosted | chart .tgz per version through the normal upload path (index regenerates) |
| r hosted | cran hosted | source + binary tarballs at the same paths (PACKAGES regenerates) |
| docker hosted | oci hosted | registry-protocol copy: manifest walk (indexes recurse), blobs before manifests, manifest bytes verbatim so digests survive |
| any proxy | same-format proxy | **configuration only** — upstream URL. Caches are self-healing and refill on demand. |
| any group | same-format group | membership (skipped members drop out) |

All content lands through forge's own format handlers in-process, so every
side effect of a real publish (checksum records, packuments, DESCRIPTION
parsing, chart records) happens exactly as if a client had uploaded it.

**Resume:** re-applying after an interruption is safe and cheap — assets
already present on the target (blob/record/manifest existence, content
address for OCI) are skipped, never re-copied.

## Permissions

- **Privileges → grants.** `nx-repository-view-*` actions map browse/read →
  `read`, add/edit → `write`, delete → `delete`; `nx-repository-admin-*` →
  `admin`; `nx-all` → global admin. Format-wide wildcards expand to the
  migrated repos of that format.
- **Content selectors** with simple path expressions
  (`path =~ "^/org/acme/.*"`) become forge selectors (`org/acme/**`) on the
  grant. Anything beyond that subset (disjunctions, coordinate terms, real
  regex) is **refused, not approximated**, and listed per role.
- **Roles** (nested roles flattened) become forge custom roles carrying
  those grants; sessions minted for users holding the role use them
  directly. `nx-admin`/`nx-anonymous` are skipped — forge's built-in
  Administrator and per-repo anonymous read cover them.
- **Users** import **disabled and without passwords** (Nexus never exposes
  password hashes). Activate each with
  `PUT /api/v1/users/{name} {"password":"…","disabled":false}` — or rely on
  LDAP/OIDC login, which needs no local password. Users holding `nx-admin`
  map to Administrator; multi-role users get a synthetic union role
  (`{user}-roles`). The source `admin` and `anonymous` users are skipped.
- **Anonymous access**: if enabled on the source, the anonymous role's read
  privileges become `AnonymousRead` on the matching forge repos.

## Not migrated (by design)

Nexus cleanup policies, scheduled tasks, routing rules, webhooks, LDAP
config, blob-store layout, and application-level privileges (settings,
script execution). The plan says so up front; recreate what you need with
forge's own cleanup policies, webhooks, and config-as-code.

## Validation harness

`scripts/nexus-migrate-validate.sh` runs the whole story against a real
`sonatype/nexus3` container: seeds all five formats over real protocols
(including a `docker push`), plans, applies, and asserts counts match +
FULL verify intact per repo + permissions live + a resume that copies
nothing.
