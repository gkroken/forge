# Live trial: forge as an organization would run it

A working install driven end to end the way a company would: real Keycloak SSO,
config-as-code in a git repository, a CI service credential, real `npm publish`
and `npm install`, and an offboarding.

Not a simulation of the clients — the actual ones. Every result below came from
a running server.

## The setup

| Piece | What it was |
|---|---|
| IdP | Keycloak 26 in Docker, realm `acme`, groups `forge-admins` / `forge-devs` / `forge-ci` / `forge-readonly` |
| People | alice (admin), bob (dev), carol (read-only) — no accounts created in forge |
| forge | `-auth` + `-oidc-*` + `-config … -config-watch` |
| Config | a git repo; a poller syncs the file in, which is the sidecar pattern |
| Clients | `npm publish` / `npm install` from `node:22-slim`, `curl` as a browser |

Forge does not clone git — `-config` is a file path. Git is the source of truth
and something else delivers the file (CI job, init container, sidecar, or the
Flux/Argo manifests in `deploy/gitops/`). Worth being explicit about, because
"config as code" can imply the server pulls.

## What worked

- **SSO against a real IdP, first try.** Discovery, the authorization-code
  redirect, JWKS, the `groups` claim, session cookie. Every OIDC test in the
  repo talks to a hand-rolled `httptest` stub, so this was the first real
  identity provider forge had ever seen.
- **Group → role mapping.** alice reached the admin API, bob and carol were
  refused it, and each landed with the right authority having never been created
  in forge.
- **Config as code.** `-config-check` validated the file the way PR CI would;
  the merged change converged into a running server without a restart; the new
  repository, retention policy and group membership all appeared.
- **The config lock.** Editing a config-managed repository through the API
  returns 409 with a genuinely good body — it names the file, the managing
  source, and the remedy including the break-glass flag.
- **Dependency confusion.** Publishing an internal `is-odd` made the group serve
  only the internal version, with `dist-tags.latest` pointing at it. The public
  versions disappeared from the merged packument entirely.
- **Offboarding.** Removing carol's group in Keycloak revoked her artifact
  access on her next login, with no action inside forge.
- **Audit.** Requests are recorded with the SSO identity (`oidc:alice@acme.example`),
  method, path and status, denials included.

## What broke

### 1. npm path traversal → cross-repository artifact poisoning · FIXED

npm publish sends the tarball in `_attachments`, keyed by filename, and publish
used that key to build the blob path. A URL is cleaned by `net/http` before
routing — which is why the same probe against maven and cran 404s — but nothing
cleans a JSON field.

A token with write access to **one** repository published:

    "_attachments": { "../../../npmjs/is-odd/-/is-odd-3.0.1.tgz": {...} }

and overwrote a cached artifact in the `npmjs` **proxy**, which the `npm` group
then served to everyone. Verified: the poisoned bytes came back from both the
proxy and the group. That is arbitrary content executing on any machine running
`npm install`, from a principal who could publish a single internal package, and
it crosses the repository boundary the grants model exists to enforce.

Fixed by deriving the key canonically and rejecting separators in attachment
names, plus containment in `Context.Key()` so no future handler taking a path
from a request body can repeat it.

### 2. Scoped packages published but could not be installed · FIXED

Same line, second bug. npm names a scoped attachment `@acme/toolkit-1.0.0.tgz`
while the packument advertises `toolkit-1.0.0.tgz`. Storing under the attachment
name put the tarball where no client looks: `npm publish` returned success and
`npm install` 404'd.

Every existing test used unscoped names — `matrixpkg`, `is-odd` — which is why
nothing caught it, and `@scope/name` is how organizations publish internally.
The repo-kind matrix missed it for the same reason.

### 3. A private repository's inventory was readable anonymously · FIXED

With every repository `anonymousRead: false`, four endpoints returned 200 to an
unauthenticated caller: `/api/v1/repos/{repo}/components` and the three
`/ui/browse/{repo}/*` JSON endpoints. Downloads were refused, so no bytes
leaked — the package names and versions did.

The dispatch comment read "browse endpoint, no admin required", and the code
implemented that as no check at all. *Not admin-only* and *not authenticated*
are different statements.

It matters more than an inventory leak usually would: those names are the target
list for the dependency-confusion attack that group shadowing exists to defeat.
Shadowing defends the pull; nothing was defending the reconnaissance.

### 4. Publisher-controlled text corrupted three generated indexes · FIXED

After the npm traversal, the same shape was worth looking for everywhere: a
document assembled by string formatting out of content a publisher controls.
Every format publishes an index, each in a different language, and the results
line up exactly with how each one is built.

| Format | Index language | Built with | Result |
|---|---|---|---|
| npm | JSON | `encoding/json` | safe |
| OCI | JSON | `encoding/json` | safe |
| PyPI | HTML | hand-rolled | was vulnerable, fixed earlier |
| **Helm** | YAML | hand-rolled | **broken** |
| **CRAN** | DCF | hand-rolled | **broken** |
| **Maven** | XML | hand-rolled | **broken** |

Every format that hand-rolls its index into a structured language had an
injection bug. Every format that hands the job to an encoder did not.

- **Helm.** Name and version came from `Chart.yaml` inside the archive and were
  interpolated unquoted. A chart named `evil: injected` made `index.yaml`
  unparseable, and `helm repo add` then failed for **every** client of that
  repository until an admin found and removed it. `description` had been quoted
  earlier for precisely this reason — the fix had covered the field that broke
  rather than the class.
- **CRAN.** A blank line ends a DCF record, but the scanner skipped blank lines,
  so a second record in the same `DESCRIPTION` overwrote the first. A package
  uploaded as `victim_1.0.0.tar.gz` was indexed as a phantom whose tarball 404s,
  while the real package vanished from `PACKAGES` despite sitting on disk.
- **Maven.** `buildMetadataXML` escaped nothing. A version directory containing
  `<` produced malformed `maven-metadata.xml`, breaking resolution for every
  Maven client.

Each is now guarded at both ends where it makes sense — rejected at upload,
neutralised at render — because records written before a guard exists still
reach the renderer, and a proxied upstream can send anything.

Worth noting what the earlier npm fix bought: helm's chart name was also a
traversal, and `Context.Key` containment had **already** neutralised it before
anyone went looking. Putting that guard in the spine rather than in npm paid for
itself within the hour.

### 5. Immutable repositories could be mutated · FIXED

`immutable: true` stopped an overwrite in place and nothing else. Publish,
DELETE (204), publish the same coordinate again with different bytes (201) — the
artifact changed while the repository still reported itself immutable.

`blob.Immutable` wrapped only `Put`. The server does refuse deletion on
immutable repos, but at the admin API; a protocol-level
`DELETE /repository/{repo}/{path}` runs through the format handler to the store,
which passed it through.

The reason it survived is the interesting part: **there was a test, and it
asserted the bypass.**

    // Delete then re-put is allowed at the store level (the server blocks
    // delete on immutable repos; the wrapper only guards live overwrites).

An assumption about a sibling component, written down as expected behaviour and
true of only one of the two routes that reach the bytes.

### 6. OCI accepted manifests referencing content it did not hold · FIXED

The spec calls for `BLOB_UNKNOWN`; forge returned 201, listed the tag, served
the manifest, and failed the pull at layer-fetch time — an image the registry
advertises that nobody can pull. Integrity verify did report both missing blobs
afterwards, so it was a robustness gap rather than a silent one.

## Subsystems that held up under attack

Probed and found sound, recorded so nobody re-tests them blind:

- **Webhook SSRF.** Every bypass refused: `localhost`, `[::1]`, decimal IP
  (`2130706433`), `127.1`, `0.0.0.0`, `169.254.169.254`, RFC1918, `file://`, and
  `localtest.me` — a *public* DNS name resolving to loopback, which proves the
  guard resolves and checks the resolved address rather than the literal string.
  A `Control` hook on the dialer re-checks the concrete address at dial time,
  so DNS rebinding and redirects to internal hosts are caught too.
- **Webhook secrets and signing.** Creation echoes the secret you just supplied;
  the list endpoint returns `""`. Signature is
  `HMAC-SHA256(timestamp + "." + body)` — the replay-resistant construction.
  Webhook creation is admin-only.
- **CSRF.** The SSO session cookie is `HttpOnly; SameSite=Strict; Max-Age=28800`,
  and `Secure` in a secure context. Strict means a browser never sends it
  cross-site.
- **Public group over a private member.** Refused in both directions — creating
  the group, and flipping an existing member private afterwards — each with a
  message naming the offending pair.
- **Token lifecycle.** Revocation takes effect on the next request (200 → 401);
  an already-expired token is refused; selector-scoped grants confine writes to
  their path (`com/acme/allowed/**` 201, everything else 403).
- **Quota** (507 at the limit), **retention** (`?dry=true` previews with reasons
  and deletes nothing; the real run drops snapshots and keeps releases), and the
  **vulnerability gate** against live OSV data — `warn` returns 200 with
  `X-Forge-Vulnerabilities`, `block` returns 403 naming the advisory.
- **OCI digests.** Content not matching its digest is refused with
  `DIGEST_INVALID`; malformed and traversing digests are refused.

## Friction worth fixing, but not bugs

- **No self-service credentials.** The token API is admin-only for create, list
  and revoke, so every developer who wants to run `npm install` against a
  private registry must ask an admin for a token — a bottleneck at any size, and
  tokens get shared as a result. `auth.Token` already carries an `Owner` field;
  the model anticipates ownership, the endpoint does not.
- **Config-defined roles cannot be attached to tokens.** The platform team
  defines `ci-publisher` with grants in git, but `createTokenRequest` takes
  `grants` and has no role reference, so every CI token restates them inline and
  drifts from the definition the moment the role changes.
- **`-config-check` cannot show a delta against production.** Run against an
  empty data directory it reports "create everything", which is not the question
  a PR reviewer is asking. The live drift endpoint answers it, but the
  pre-merge check does not.
- **~60s convergence with no way to force it.** `-config-watch` polls, so an
  admin merges and then waits with no feedback. The "re-applied after file
  change" line also logs with all-zero counts when nothing changed, which reads
  as "your change did nothing" — it briefly convinced me the watcher was broken
  before the next cycle applied it.
- **A destructive endpoint ignores unknown query parameters.** I previewed a
  retention run with `?dryRun=true`, which is not a parameter — the real one is
  `?dry=true` — so it performed a live cleanup and deleted two artifacts while I
  believed I was previewing. My mistake, and the docs are correct, but a typo
  like `?dry=ture` behaves identically. Rejecting unrecognised query parameters
  with a 400 on the cleanup endpoint would turn a silent data-loss footgun into
  an error.
- **Group shadowing is all-or-nothing.** Once `is-odd` exists internally, the
  group serves no upstream version of it. That is the deliberate and safe
  policy, consistent across formats, but a team that vendors one patched version
  of a public package loses access to the rest through that URL, and should know
  it going in.

## A setup trap worth documenting

Keycloak's group mapper defaults to full paths, emitting `/forge-devs`. Forge's
mappings are configured as `forge-devs:write`. Set `full.path: false` on the
mapper — or every SSO user lands with no role and the cause is invisible from
forge's side.
