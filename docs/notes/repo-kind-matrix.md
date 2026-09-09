# Repo-kind matrix: what every format must do for every repository kind

Run it with `scripts/repo-kind-matrix.py` against a live server:

```bash
go build -o forge ./cmd/forge
rm -rf ./data && ./forge -addr :8099 -data ./data &
python3 scripts/repo-kind-matrix.py
```

Proxy and group checks talk to the real upstreams, so the run needs network
access. Last run: **116 passed, 2 failed** — both remaining failures are OCI
(F2, F3). F1 is fixed.

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

### Group

| ID | Expected |
|---|---|
| G1 | An artifact held by a hosted member is served |
| G2 | An artifact/index from a proxy member is served |
| G3 | An artifact no member has returns 404 |
| G4 | Publishing to a group is refused |
| G5 | A dead proxy member does not take the group down |
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

### F2 — OCI group repositories are silently empty

`internal/format/oci/oci.go` · `Serve`

`Serve` branches on `repo.Proxy` and otherwise falls through to the hosted
paths; there is no group branch. A group is accepted by the admin API without
complaint and then answers `404 MANIFEST_UNKNOWN` for an image its own member
holds — the member returns 200 for the same request. Nothing anywhere says the
kind is unimplemented.

This is the silent-absence pattern: the repository looks real in the API and the
UI and simply serves nothing.

### F3 — OCI proxy cannot pull from Docker Hub

`internal/format/oci/oci.go` · `proxyPass`

The pass-through works against registries that allow anonymous access —
`registry.k8s.io/pause:3.9` returns 200 through forge. Docker Hub returns
`401 UNAUTHORIZED`, because it requires a token handshake against
`auth.docker.io` that `proxyPass` does not perform. Docker Hub is the upstream
most people would configure first.

### F4 — the docs say OCI proxy is unsupported, and it is not

`README.md` marks OCI as proxy `—`, but proxy mode is implemented and works
against token-free registries (F3). The row understates what exists while F2/F3
overstate how far it goes. Both should say what is true.

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
