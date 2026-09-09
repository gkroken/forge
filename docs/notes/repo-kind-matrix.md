# Repo-kind matrix: what every format must do for every repository kind

Run it with `scripts/repo-kind-matrix.py` against a live server:

```bash
go build -o forge ./cmd/forge
rm -rf ./data && ./forge -addr :8099 -data ./data &
python3 scripts/repo-kind-matrix.py
```

Proxy and group checks talk to the real upstreams, so the run needs network
access. Last run: **123 passed, 0 failed**, and the script is re-runnable
against the same server. F1–F6 are all fixed; the findings are kept below
because how each one hid is more useful than the fix.

## The checklist

These are behaviours a repository kind owes its clients regardless of format.
The format only decides which URL expresses them.

### Hosted

| ID | Expected |
|---|---|
| H1 | Publishing an artifact succeeds |
| H2 | The format's index/listing reflects the publish |
| H3 | Download returns byte-identical content |
| H3b | Checksum sidecars are served where the ecosystem expects them (Maven) |
| H4 | The components API lists the component |
| H5 | Integrity verify runs and reports nothing on a clean repo |
| H6 | The browse endpoints answer |
| H7 | Delete is accepted, the artifact stops downloading, and the index drops it |

### Proxy

| ID | Expected |
|---|---|
| P1 | A real upstream artifact is fetched on a cache miss |
| P2 | A second read is consistent with the first (served from cache) |
| P3 | URLs inside returned metadata point at forge, not upstream |
| P3b | No upstream host leaks into rewritten metadata |
| P4 | An artifact upstream does not have returns **404** |
| P5 | Publishing to a proxy is refused |
| P6 | A missing artifact is negative-cached, so a second lookup does not hit upstream |
| P7 | Browse/components lists only what is cached, not the upstream catalogue |
| P8 | Distinct content and metadata ages are accepted and both are honoured |

### Group

| ID | Expected |
|---|---|
| G1 | An artifact held by a hosted member is served |
| G2 | An artifact/index from a proxy member is served |
| G3 | An artifact no member has returns 404 |
| G4 | Publishing to a group is refused |
| G5 | A dead proxy member does not take the group down |
| C1/C2 | OCI `_catalog` lists images, merges members, and pages with `n`/`last` |
| S1/S2 | **A hosted member shadows a proxy member offering the same name** |

S1/S2 is the dependency-confusion property and the most important row in this
table: it is the difference between "internal package wins" and "anyone who
registers your internal name on a public registry owns your builds."

## Results

Everything passes for maven, npm, helm, cran and pypi across hosted, proxy and
group — **including S1/S2 shadowing for all five**, verified against the real
public registries (Maven Central, npmjs, CRAN, Bitnami, pypi.org). Two checks
still fail, both in OCI. F1 (npm proxy) is fixed; it is kept below because the
way it hid is more useful than the fix.

### F1 — npm proxy: 404 became 502, nothing was negative-cached, and cached packuments never expired · FIXED

`internal/format/npm/npm.go` · `fetchPackument` / `proxyPackument`

`fetchPackument` returned a bare `bool`, so "upstream said 404" and "upstream is
unreachable" were the same value, and `proxyPackument` mapped both to
`502 upstream unavailable`. Every other format returns 404 here.

Two consequences, the second worse than the first:

- npm clients see a registry error instead of "not found".
- Nothing wrote a negative-cache entry, so **every** lookup of a nonexistent
  package went to the upstream registry. Maven and PyPI both pass P6; npm is
  the only format that re-asks upstream forever. A typo'd dependency in CI
  hammers npmjs.org on every build.

Root cause is structural: the packument path predates the shared
`proxy.Fetcher` and does its own HTTP, so it has none of the fetcher's negative
caching, circuit breaking, or coalescing. The tarball path a few lines below
uses the fetcher and handles `proxy.ErrNotFound` correctly.

**F1b, found while writing the regression test for F1 — and worse than it.**
`packument()` returned any stored copy immediately, for proxies too. Everything
in `fetchPackument` — the TTL check, the conditional GET, ETag revalidation,
stale-on-error — only ran when *nothing* was cached. So a proxied packument was
permanent: versions published upstream after the first fetch stayed invisible
for the life of the cache. A test that ages the cache entry past its TTL and
counts upstream requests measured exactly one hit, where there should have been
a revalidation.

This is why the matrix's P2 ("second read consistent") passed while the proxy
was badly broken: a check that a cache *returns the same bytes* cannot tell a
working cache from one that never expires.

**Fix.** `fetchPackument` now returns an `error`, so `proxy.ErrNotFound` and
"upstream unreachable" are distinct; a 404 writes a negative-cache entry and is
answered locally for the negative-TTL window; and `packument()` routes proxy
reads through `fetchPackument` so the TTL path actually runs. A 404 is treated
as authoritative and is not served from a stale copy, matching the shared
Fetcher exactly rather than inventing a second policy. `proxy.Config` gained
`EffectiveNegativeTTL()` so the two paths share one default.

### F2 — OCI group repositories were silently empty · FIXED

`internal/format/oci/oci.go` · `Serve`

`Serve` branches on `repo.Proxy` and otherwise falls through to the hosted
paths; there is no group branch. A group is accepted by the admin API without
complaint and then answers `404 MANIFEST_UNKNOWN` for an image its own member
holds — the member returns 200 for the same request. Nothing anywhere says the
kind is unimplemented.

This is the silent-absence pattern: the repository looks real in the API and the
UI and simply serves nothing.

#### Map

Most of this is already built: `format.GroupFetch` and `format.GroupMerge`
landed with PyPI groups, and OCI needs no new policy — only the three
operations wired to them.

| Operation | Behaviour | Mechanism |
|---|---|---|
| `GET/HEAD {image}/manifests/{ref}` | first member that has it wins | `format.GroupFetch` |
| `GET/HEAD {image}/blobs/{digest}` | first member that has it | `format.GroupFetch` |
| `GET {image}/tags/list` | union of member tag lists | `format.GroupMerge`, keyed (image, tag) |
| any write | 405 | reject before dispatch |

Notes that decide the design:

- **Blobs need no shadowing.** A blob is addressed by its own digest, so member
  order changes latency, never correctness. Shadowing matters only for
  tag-addressed reads: `myorg/app:latest` published internally must beat a
  public image of the same name, which is the container-image form of the
  dependency-confusion property S1/S2 already pins for the other five formats.
- **`tags/list` is the only merge.** It is also the only place a hosted tag can
  hide a proxy tag, so it is where `GroupMerge`'s `hostedNames`/`NameClaimed`
  rules actually apply.
- **A manifest and its blobs may come from different members.** That is fine
  and needs no special handling: the client requests each by path, and
  `GroupFetch` independently finds whichever member holds it.
- **`_catalog` is not implemented for any kind** — it is absent from the op
  switch entirely, so it 404s as "unknown OCI operation". Worth deciding
  separately from groups.

Effort: small. The work is a `serveGroup` in the OCI handler plus a tags/list
renderer; no new spine helpers.

#### What the map got wrong

Writing the tests changed one decision. The map said blobs need no shadowing
(true) and implied tag-level merging was enough (false). Shadowing only the
merged `tags/list` is theatre: `docker pull` resolves a tag directly and never
reads the listing, so an "hidden" upstream image would still pull.

Ownership is therefore applied at the **image name**: once a hosted member holds
a name, proxy members stop answering for that name for both `tags/list` and
`manifests`, via `MemberFilter` — which exists for exactly this. Blobs stay
exempt, because they are digest-addressed and a hosted manifest may legitimately
reference layers a proxy member cached. Ownership bites only for names a hosted
member actually holds, so a group is still a proxy for everything else.

### F3 — OCI proxy could not pull from Docker Hub, and cached nothing · FIXED

`internal/format/oci/oci.go` · `proxyPass`

Two separate problems, and the second is the larger one.

**F3a — no auth-token handshake.** The pass-through works against registries
that allow anonymous access: `registry.k8s.io/pause:3.9` returns 200 through
forge. Docker Hub returns `401 UNAUTHORIZED` because it requires a token
obtained from `auth.docker.io`, which `proxyPass` never requests. Docker Hub is
the upstream most people would configure first.

**F3b — it is a reverse proxy, not a cache.** `proxyPass` builds an upstream
request, forwards three headers and `io.Copy`s the body to the client. It never
touches `blob.Store` or `meta.Store`. Measured: after pulling a manifest and a
config blob twice through an OCI proxy, the repository had **0 cached blobs and
no cache entries**, while `maven-central` had cached its artifacts after one
fetch. So every pull goes upstream, and the proxy provides no rate-limit
relief, no offline resilience, no stale-on-error and no negative caching — the
reasons to run one in front of Docker Hub in the first place.

Note this is the same shape as F1b: a check that a second read returns the same
bytes cannot distinguish a cache from a passthrough. `P2` passed here too.

#### Map

**F3a — token handshake** (independent, unblocks Docker Hub):

1. On a 401 from upstream, parse `WWW-Authenticate: Bearer realm=…,service=…`.
2. `GET {realm}?service={service}&scope=repository:{image}:pull`, sending the
   repo's `ProxyAuth` as Basic credentials when set (that is also how private
   upstream images work).
3. Retry the original request with `Authorization: Bearer {token}`.
4. Cache tokens per (upstream, scope) until `expires_in`. This is the standard
   Docker Registry v2 flow, so it also covers ghcr.io and quay.io.

**F3b — real caching** (the bigger win). Route proxy reads through the shared
`proxy.Fetcher`, as every other format does, which brings TTL, ETag
revalidation, negative caching, stale-on-error, request coalescing and the
circuit breaker at once. The cache keys differ by mutability, and getting that
distinction wrong is exactly the F1b bug:

| Read | Mutability | Caching |
|---|---|---|
| `blobs/{digest}` | immutable | cache forever, key `{repo}/blobs/{digest}` |
| `manifests/{digest}` | immutable | cache forever |
| `manifests/{tag}` | **mutable** | TTL + revalidation — a tag that never expires pins `:latest` to whatever was first pulled |
| `tags/list` | mutable | short TTL |

Do F3b first if only one gets done: it is the reason a proxy exists, and it
applies to the registries that already work today.

#### Outcome

Both done. F3a walks the Bearer challenge from `/v2/`, exchanges it for a pull
token and caches tokens per (upstream, image) until `expires_in`; configured
`ProxyAuth` authenticates the token request itself, which is how private
upstream images work. F3b routes every proxy read through `proxy.Fetcher`.
Measured after: Docker Hub `library/alpine` pulls 200, the repo holds cached
blobs and cache entries where it held none, and a second pull is 0.5ms against
159ms cold.

Two things the implementation needed that the map missed:

- **`proxy.Fetcher` could not send request headers**, and for OCI the `Accept`
  header decides whether a registry returns an image manifest or a
  multi-platform index. `Config.Headers` was added for this. Tag-addressed
  manifests are also cached per-`Accept` (a hash of the header is in the key),
  because one key per tag would hand one client the media type another asked
  for.
- **`MetadataMaxAge` was dead configuration.** The admin API accepts and stores
  it, but `proxy.ConfigForRepo` only ever reads `ContentMaxAge`, so no format
  consulted it. OCI now uses it for the mutable reads (tags, tag manifests).
  **It remains unused by every other format** — a separate silent no-op worth
  its own fix.

### F4 — the docs said OCI proxy was unsupported, and it was not · FIXED

`README.md` marked OCI as proxy `—` while proxy mode was implemented and worked
against token-free registries. With F2 and F3 done the row is now `✅ ✅ ✅`,
which is finally true of all three kinds.

## Test bugs found while writing this

Recorded because each one initially looked like a product bug:

- **Maven delete.** Deleting `widget-1.0.0.jar` left `1.0.0` in
  `maven-metadata.xml`. That is correct — the version directory still held the
  `.pom`, and Maven aggregates over files present. After deleting both, the
  metadata 404s. The check now removes both.
- **OCI proxy probe.** A 401 from Docker Hub is a real upstream response, not a
  forge failure; the capability check now uses a token-free registry, and
  Docker Hub is asserted separately as F3.
- **PyPI group resilience.** Failed only because the delete phase had removed
  the fixture it reused. Phases share state, so later checks must not reuse
  names earlier phases delete.

### F5 — MetadataMaxAge was dead configuration · FIXED

`proxy.ConfigForRepo` only ever read `ContentMaxAge`, so the `metadataMaxAge`
the admin API and config files accepted did nothing for any format. The two
ages exist because they age differently: an artifact at a fixed coordinate never
changes, while the index listing it grows on every upstream publish. One TTL
forces one of them to be wrong.

`Context.ProxyMetadataConfig()` now applies it, and every format uses it for its
index read — maven-metadata.xml, the npm packument, Helm's index.yaml, CRAN's
PACKAGES*, PyPI simple pages, and OCI tags/manifest-by-tag — while artifact
reads keep `ContentMaxAge`. An unset or zero value falls back to the content
age rather than meaning "expire immediately".

Found while fixing F3, where the same distinction decides whether `:latest`
pins to the first pull.

### F6 — OCI `_catalog` was unimplemented · FIXED

It was absent from the op switch, so `GET /_catalog` answered "unknown OCI
operation" for all three kinds. Now: hosted lists its image names, a group
merges its members (a member that refuses to list contributes nothing rather
than failing the group), and a proxy passes through. `n`/`last` pagination and
the `Link: rel="next"` header are implemented, since clients page rather than
asking for an entire registry, and an empty registry renders `[]` rather than
`null`.

## A flaw in this harness, fixed

The first version could only run once against a server: it created repositories
and then failed with 409 on a re-run, and its negative-cache check compared the
second lookup's latency against the first, which only holds from a cold cache.
Both are fixed — setup treats 409 as "already there", and the cache check
accepts either a clear speed-up or an absolutely-local second lookup. The
script now passes 123/123 cold and warm.

## helm proxy was serving nothing at all (2026-09-09)

`GET /index.yaml` on a helm **proxy** repository rendered the *local* records —
which on a proxy are always empty — so `helm repo add` succeeded, `helm search`
found nothing, and no chart could ever be pulled. `.tgz` reads on a proxy went
to the local blob store only, and nothing ever wrote to it. The UI was fine
(`BrowseRepo` did consult upstream), which is why this survived: every check
looked at the repository through forge's own pages rather than through helm.

The matrix passed it because P1 asked only for a **200**, and an empty index is
a 200. The suite now asserts the index lists charts (P1a), that its links point
at forge rather than upstream (P1b), and that a chart it lists actually
downloads through forge (P1c). Verified with the real `helm` CLI: `repo add`,
`search repo`, and `pull` against both a proxy and a group.

Fixed by making `index()` kind-aware, rewriting upstream `urls:` entries to the
bare filename (helm resolves relative entries against the repository URL, so
the download comes back to forge), and serving the chart through
`proxy.Fetcher` using the URL upstream published — never one synthesised from a
filename. Group downloads now go through `format.GroupFetch`, so a group with a
proxy member serves upstream charts too.

Also: the seeded `helm-proxy` pointed at `charts.bitnami.com`, which now
redirects to Broadcom and publishes `oci://` references instead of `.tgz` URLs
— an HTTP chart proxy cannot serve those. The seed is
`prometheus-community.github.io/helm-charts`, a plain chart repository.
