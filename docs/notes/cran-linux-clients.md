# CRAN Linux binaries — how real R clients behave (Phase L0)

Evidence for `WORKPLAN-CRAN-LINUX.md` §3 L0. Every claim below comes from a run,
not from documentation, unless marked *(source read)*.

**Setup (2026-10-10).** `rocker/r-ver:4.6.1` (Ubuntu 24.04 noble, amd64) plus
renv 1.3.1, pak 0.11.1, remotes 2.5.0 and the `curl`/`wget` CLIs. Clients pointed
at a request-logging HTTP server on the host that served either static fixture
trees or a reverse proxy to `cloud.r-project.org` / `p3m.dev`, so every request
path and User-Agent was recorded. Test packages: `jsonlite` 2.0.0 (compiled C)
and `praise` 1.0.0 (pure R). "Installed a binary" was proven by the installed
`Built:` timestamp equalling the fixture's build time, and by installing with
`gcc`/`make` hidden from `PATH`.

---

## 1. User-Agent per client

| Client / method | User-Agent sent | R version + arch? |
|---|---|---|
| `install.packages`, rocker profile (libcurl) | `R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)` | yes |
| `install.packages`, `--vanilla` (libcurl) | `libcurl/8.5.0` | **no** |
| `install.packages`, `download.file.method="curl"` | `curl/8.5.0` | **no** |
| `install.packages`, `download.file.method="wget"` | `Wget/1.21.4` | **no** |
| `download.packages`, rocker profile | same as `install.packages` | yes |
| renv, `RENV_DOWNLOAD_METHOD` = libcurl / curl / wget | `renv (1.3.1); R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)` | yes, all three |
| renv, `--vanilla` | `renv (1.3.1); R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)` | yes |
| pak (rocker profile or `--vanilla`) | `R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)` | yes |
| renv's `__api__/status` probe | the R option value, *without* the `renv (…)` prefix | yes |

**Why vanilla R sends no version.** *(source read, R 4.6.1
`src/modules/internet/libcurl.c` lines 287–303)* — if `HTTPUserAgent` starts with
`R (`, which is R's own default, R's libcurl method replaces it with
`libcurl/<curl version>`. Setting the option to that same default string explicitly
changes nothing (measured). Only a value that does *not* start with `R (` reaches
the server, which is why rocker's `Rprofile.site` sets
`R/x.y.z R (x.y.z platform arch os)` (a format Posit also documents). It is a
client-side R option only: the agent goes to whatever repo the client uses —
forge — and involves no third-party service.

Consequences:

- renv and pak always identify R version and arch, whatever download method.
- Plain `install.packages` identifies them only with an `HTTPUserAgent` that
  does not start with `R (` set (rocker images do this by default). Without it, and with
  the curl/wget methods, the request is indistinguishable from `curl`.
- renv does not override `HTTPUserAgent`, it prefixes it; pak ignores the option
  and sends its own. Neither sends anything else identifying (no custom headers).
- The parseable core in every R-originated UA is the substring
  `R (<x.y.z> <platform triple> <arch> <os>)`.

## 2. How renv and pak recognise PPM-style URLs

**renv** *(source read: `renv_ppm_transform_impl`, then confirmed by runs)*:

- A repo URL that already contains `/__<word>__/` is used verbatim.
- Otherwise, for an `http(s)` URL with ≥ 2 path segments that is not a known CRAN
  mirror, renv GETs `dirname(dirname(url))/__api__/status`. If the JSON lists a
  distro with `binaryURL` equal to the client's codename (`noble`) and
  `binaries: true`, renv **rewrites** the URL to
  `dirname(url)/__linux__/noble/basename(url)` — PPM's layout, where the repo name
  is the *last* segment.
- For forge's `http://forge/repository/cran-public/` that means probing
  `http://forge/__api__/status` and rewriting to
  `http://forge/repository/__linux__/noble/cran-public/`.
- **Hazard, measured:** when the status endpoint answered PPM-style but the
  rewritten path did not exist, `renv::install` failed outright (404 on every
  `PACKAGES` variant, no fallback to the original URL). Today forge answers
  `/__api__/status` with a plain-text 404, renv leaves the URL alone, and source
  installs work (verified against a running forge).
- An explicit `…/__linux__/noble` or `…/__linux__/noble/latest` URL reported
  `[installed binary]`, and needed no compiler.

**pak** *(pkgcache, source read, then confirmed by runs)*:

- pak never rewrites an arbitrary repo URL. It probes `__api__/status` only on its
  own PPM base (`PKGCACHE_PPM_URL`, default packagemanager.posit.co) for `PPM@…`
  repo sugar.
- A repo URL is treated as a PPM binary repo only if it matches
  `^(base/)(repo)/__linux__/(distro)/(latest|[-0-9]+)$`. The distro segment is
  `[a-zA-Z0-9_]+`, so no `-` or `.`. For such a URL pak also fetches
  `{base}{repo}/{latest}/bin/linux/{rminor}-{distro}/contrib/{rminor}/PACKAGES.gz`.
  That file lists the packages that have a binary, and pak labels exactly those
  `x86_64-pc-linux-gnu-ubuntu-24.04` instead of `source`.
- On any other URL (plain, or `__linux__/noble/` without `latest`) pak labels
  every package `source` and runs a "Building" step on what it downloads. When
  the bytes are a binary, that step just repackages it (no compile, `Built:`
  unchanged). **But with no compiler on the machine it fails** ("error in pak
  subprocess"), so on a runtime image without build tools pak needs the full PPM
  shape plus the binary index.
- **Measured:** if the binary index lists a package but the tarball served is
  source, pak compiles it when a compiler exists and **fails** when none does. The
  index and the tarball must therefore come from the same decision.
- pak adds no `Archive/` fallback for `__linux__` URLs (on plain URLs it fetches
  `src/contrib/Archive/{pkg}/…` in parallel with the current path).

**What a lockfile records.** `renv::snapshot` with an explicit
`…/__linux__/noble` URL records that URL verbatim under `R.Repositories[].URL`,
and `"Repository": "CRAN"` (the name) per package. `renv::restore` from that
lockfile in a fresh container used the URL as recorded and installed the binary.
renv's `renv_restore_normalize_impl` only rewrites per-package `Repository` values
that are URLs, and the name `CRAN` is not one. A lockfile is therefore
distro-specific through the URL, exactly as it is with PPM.

## 3. Serving a binary on the source path; what PPM's index contains

- **A Linux binary tarball served on the plain `src/contrib/{pkg}_{ver}.tar.gz`
  path installs as a binary:** `install.packages` (any UA) printed
  `* installing *binary* package 'jsonlite'` and kept the fixture's `Built:`
  timestamp. It also installed with `gcc`/`make` hidden. A real source tarball
  under the same conditions failed (`make: not found`), the control.
- **PPM's `__linux__/noble/latest/src/contrib/PACKAGES*` is the plain source
  index** — no `Built`, no `Platform`, no `Archs` — and identical for every UA.
  It sends `cache-control: max-age=3600` and **no `Vary`** header.
- **PPM picks binary vs source per tarball request, from the UA alone,** with a
  307 to a content-addressed blob. Response headers: `x-package-type:
  binary|source`, `x-package-binary-tag: 4.6-noble`.
  - `R/4.6.1 R (… x86_64 …)` → `4.6-noble`.
  - `R/4.5.1 …` → `4.5-noble`.
  - `aarch64` → `4.6-noble-arm64`.
  - Bare `R (4.6.1 …)` sent by curl → binary (PPM parses that form too).
  - `libcurl/8.5.0` → source.

  So PPM caches UA-dependent responses for an hour without `Vary`.
- PPM's pak index (`latest/bin/linux/4.6-noble/contrib/4.6/PACKAGES.gz`) is a
  `PACKAGES` file with a `Hash:` field per entry and no `Built`; it listed 25 280
  packages.
- PPM rewrites `Repository: CRAN` to `Repository: RSPM` in source *and* binary
  DESCRIPTIONs, so the field is mirror-specific.

## 4. Packed installed directory vs `R CMD INSTALL --build`

- `tar czf jsonlite_2.0.0.tar.gz -C lib jsonlite` installs identically to the
  `--build` output (both: `installing *binary* package`, no compile).
- They differ only by:
  - `--build` adds `jsonlite/SHA256` (a per-file checksum list);
  - the packed tarball has an explicit `jsonlite/` directory entry;
  - the `Built:` timestamp.
- `--build` names its output `jsonlite_2.0.0_R_x86_64-pc-linux-gnu.tar.gz`, so a
  helper using it must rename before upload.
- **renv libraries are symlinks** into the renv cache
  (`renv/library/…/praise -> ~/.cache/R/renv/cache/v5/…`), so the helper must
  dereference (`tar -h`, or pack from the resolved path), or it uploads a symlink.

**What an installed DESCRIPTION adds to the source DESCRIPTION** (jsonlite, measured):

| Installed by | Difference from the source DESCRIPTION |
|---|---|
| `install.packages` / `R CMD INSTALL` | `+ Built:` only; every other byte equal |
| pak | `+ Built:`, `+ RemoteType/RemotePkgRef/RemoteRef/RemoteRepos/RemotePkgPlatform/RemoteSha` |
| renv | the same plus `RemoteReposName`, **and the whole file re-serialised**: multi-line fields (`Authors@R`, `Description`, `Author`, `URL`) folded onto one line |

So the L1 "matches the source" check must compare **fields after DCF parsing, with
internal whitespace collapsed**, require every source field to be present and
equal, and allow only `Built` and `Remote*` as extras. A byte or line comparison
would reject every renv-built upload. Comparing only `Packaged`/`Date/Publication`
is weaker than necessary, and `Repository` must not be compared (mirrors rewrite
it).

---

## 5. Proposed path design (for approval — not implemented)

One URL per group, PPM's client-facing shape, so all three clients recognise it:

```
/repository/{group}/__linux__/{distro}/latest/
```

| Request under the group | Served |
|---|---|
| `__linux__/{distro}/latest/src/contrib/PACKAGES[.gz\|.rds]` | the group's merged **source** index, unchanged (as PPM) |
| `__linux__/{distro}/latest/src/contrib/{pkg}_{ver}.tar.gz` | `resolve(key)` → binary for the exact key, else source |
| `__linux__/{distro}/latest/src/contrib/Archive/{pkg}/{pkg}_{ver}.tar.gz` | same decision |
| `latest/bin/linux/{rminor}-{distro}/contrib/{rminor}/PACKAGES.gz` (pak) | only entries with a binary for the exact key, from the same decision |

**Key resolution.** `{distro}` comes from the path. R minor, arch and platform
triple come from the `R (x.y.z triple arch os)` substring of the UA, the only
thing every R client sends. The pak index path also carries the R minor; if it
disagrees with the UA, the index is empty (fail toward source). Anything
unparseable — `libcurl/…`, `curl/…`, `Wget/…`, a distro with no registered key, an
arch with no registered key — serves source. So vanilla `install.packages`
silently gets source; the fix, for forge's setup docs, is one client-side R option
(`options(HTTPUserAgent = sprintf("R/%s R (%s)", …))`), which rocker already ships.
Nothing here contacts or depends on PPM (§0 decision 1): forge only copies the URL
*shape* renv and pak recognise, on its own host.

**`Vary: User-Agent`** on the tarball and pak-index responses (PPM omits it; we
should not).

**Only `latest`.** forge has no snapshots, so a date segment is 404 rather than
silently meaning "latest".

**`/__api__/status` stays a 404.** Emulating it would make renv rewrite plain
`/repository/cran-public/` URLs to `/repository/__linux__/noble/cran-public/`. That
needs a CRAN-specific route in `server.go`, against the architecture invariant.
Any gap between what the status endpoint advertises and what that path serves
breaks renv with no fallback (measured). Users configure the explicit
`__linux__` URL instead — renv, pak and `install.packages` all accept it.

**Inside the CRAN handler, not routing.** Both public paths arrive at the group's
CRAN handler as `c.Sub` (`__linux__/…` and `latest/bin/linux/…`), so `server.go` is
untouched. Storage stays as the workplan says (`bin/linux/{distro}-{arch}/contrib/{rminor}/`
in the dedicated binary repo).

**Same decision everywhere.** `resolve` answers "binary for this key, or source",
and both the tarball path and the pak index call it. The pak/no-compiler run above
shows what happens when they disagree.

## 6. Notes for later phases

- **L1 validation:** compare DESCRIPTION fields as in §4, not bytes; never compare
  `Repository`.
- **L4 helper:** dereference renv symlinks. The packed form and the `--build`
  form are both acceptable.
- **L4 helper, open question:** binaries packed from a renv or pak library carry
  the builder's `Remote*` fields, including `RemoteRepos` (the builder's repo URL),
  into every client that installs them. `install.packages` keeps them verbatim;
  renv and pak overwrite them on install. Raise before L4: strip in the helper,
  build with `R CMD INSTALL --build` from the source tarball instead, or accept.
- **Conformance negatives (L4)** can use crafted UAs with `curl`: other minor
  (`R (4.5.1 …)`), arm64 (`R (4.6.1 aarch64-unknown-linux-gnu aarch64 linux-gnu)`),
  `libcurl/8.5.0`, and an unregistered distro path.
- **pak needs no compiler only on the full URL shape;** the conformance job should
  run each client in a container with the compiler removed, which is the sharpest
  proof that a binary was served.
