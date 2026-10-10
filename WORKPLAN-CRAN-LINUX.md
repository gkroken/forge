# forge — CRAN Linux Binary Cache

> **Status: PLANNED** — not started. Origin: Skald issue 003 (2026-10-10), "every R
> restore on Linux compiles from source". Measured there: renv restore was 155 s of a
> 253 s cold build for a 46-package app; tidyverse/sf/arrow-sized apps take minutes.

CRAN publishes Windows and macOS binaries (forge proxies and hosts those since
`WORKPLAN-CRAN-BINARY.md`), but **no Linux binaries**, because a Linux binary links
the distro's shared libraries and only works on the release it was built on. This
plan lets a *trusted builder* push the packages it compiles into forge, so each
(package, version, platform) is compiled once and every later client installs a
binary — through the one repository URL it already uses, with a silent fallback to
source on a miss.

---

## 0. Decisions already made (do not re-litigate)

These were settled in the design discussion of 2026-10-10:

1. **No third-party passthrough.** Not Posit Public Package Manager (its service terms
   reportedly forbid mirroring and exclude competitors), not r-universe, not r2u (apt
   `.deb`s, not a CRAN tree). forge's binaries come from builds the operator controls.
2. **forge does not compile, and never executes package code.** Binaries are built
   by a builder outside forge and uploaded. A forge-side build service is a possible
   *later* producer for the same tree (§8), not part of this plan.
3. **Upload is a privileged act.** Only a builder holding a write grant on the binary
   repo may upload — the Nix/Bazel remote-cache model: CI builders write, everyone
   else reads. Developer machines never write.
4. **One canonical builder image per platform key.** Binaries built in different
   environments differ (packages detect optional system libraries at configure
   time), so a platform key is bound to one builder image. For Skald the builder
   image *is* the runtime base image, so what was built there loads there.
5. **One URL for users, misses fall back to source.** The client configures a
   single repo URL; a binary is served when one exists for its exact platform key,
   otherwise the source tarball. A binary is **never** served for a key it was not
   built for — wrong-platform binaries fail at load time, which is worse than a
   compile.
6. **Binaries never go stale; deletion is storage policy.** A binary for a full key
   is correct forever. Eviction is opt-in per repo via the existing
   `LastDownloadedDays` cleanup policy; the default is to keep.
7. **Removing a bad binary is one action.** renv does not fall back to source when
   a binary fails to load, so a bad binary blocks every client on its platform until
   removed. Removal must be a single documented, audited call; the next build then
   recompiles and re-uploads.

---

## 1. Definition of Done

- A real R client in `rocker/r-ver:4.6` (Ubuntu noble, amd64), pointed at forge's
  single group URL, installs a package **from source** on first use; the builder
  helper uploads it; a **second, fresh** container installs the same package as a
  **binary** (`* installing *binary* package` in the install log, no compiler run).
  Verified for `install.packages`, `renv::restore()` and `pak::pkg_install()`.
- Mismatched clients get source, never a binary: other R minor, other arch, other
  distro path, and a request with no parseable R user agent. Each is a conformance
  check that fails if the guard is removed (mutation-tested, per `/format-live-test`).
- Uploads are rejected (with a reason) when: the tarball is not an installed-package
  layout; `Built:` disagrees with the key (R minor, platform triple); the package or
  version disagrees with the path; it does not match the source forge serves for
  that version; an ELF object's machine disagrees with the key; an ELF object needs
  a shared library or glibc symbol version absent from the key's platform manifest;
  the uploader lacks the write grant; the key is not registered.
- Overwriting an existing binary is refused (409). Eviction and explicit removal
  still work, after which a fresh upload of the same key is accepted.
- Every binary has a provenance record (uploader identity, builder image digest as
  reported, source sha256, `LinkingTo` versions, needed shared libraries) visible in
  the UI detail pane and the API, and its needed libraries are shown as the
  binary's system requirements.
- `ledger.Record` on upload, `ledger.Forget` on delete; `LastDownloadedDays` evicts
  binaries without touching source; trash/restore works.
- `go test ./...` green, coverage gates hold, `test.sh` green, the Linux binary
  conformance job green in CI, `scripts/repo-kind-matrix.py` extended and green.
- `docs/setup.md` documents builder setup, platform registration and removal.

---

## 2. Guiding principles

1. **Go stdlib only.** `debug/elf`, `archive/tar`, `compress/gzip` cover every check.
2. **Copy PPM's client-facing layout; invent nothing clients would have to learn.**
   renv, pak and rig already understand `__linux__/<distro>/` URLs. Internal
   storage reuses the existing `bin/{platform}/contrib/{rver}/` tree.
3. **Fail toward source.** Any doubt about the client's platform (unparseable user
   agent, unregistered distro, missing binary) serves source. Source is slow;
   a wrong binary is broken.
4. **Guards live in the thing they guard** (memory: guard placement). The
   never-wrong-binary decision is one function called by every read path (index,
   tarball, Archive, group), not N copies.
5. **Static checks only.** forge validates what it can read without running code
   and leaves deliberate tampering to access control and provenance (§7).

---

## 3. Phases

### Phase L0 — Client behaviour spike  *(gate; no product code)*

Answer with evidence from real clients in `rocker/r-ver:4.6` before designing paths.
Record the results in `docs/notes/cran-linux-clients.md`.

- The exact `User-Agent` sent by `install.packages`, `download.packages`, renv (each
  download method: libcurl, curl, wget) and pak, and whether each carries the R
  version and arch. PPM selects the R minor from the UA; confirm what happens when
  it is absent (and whether renv/pak override `HTTPUserAgent`).
- How renv and pak recognise PPM-style URLs: by host, by the `__linux__` path
  segment, or by probing an endpoint (e.g. `__api__/status`). Can forge be
  recognised the same way, and does renv's automatic binary-URL rewriting for PPM
  apply to it? What does an renv lockfile record as the repo URL?
- Confirm `R CMD INSTALL` installs a Linux binary tarball (one with `Built:`) served
  on the **source** path without compiling, and what `PACKAGES` fields PPM emits on
  its binary path (does it advertise `Built`/`Platform`, or just serve the binary?).
- Confirm packing an *installed* package directory (`tar czf pkg_ver.tar.gz -C lib
  pkg`) is accepted identically to `R CMD INSTALL --build` output.
- Decide from the above: the exact public path (`/repository/{group}/__linux__/{distro}/…`
  or a variant), where R minor and arch come from (UA vs path), and how a client
  with no usable UA is treated (source).

**Exit:** the note exists, every bullet answered from a real run, path design fixed.

### Phase L1 — Binary repo, platform registration, validated upload

- **Platform keys.** A key is `{distro}-{arch}/{rminor}` (e.g. `noble-x86_64/4.6`).
  A key accepts uploads only after an admin registers it with: the builder image
  reference + digest, max glibc version, and a shared-library manifest (the
  `ldconfig -p` output of the builder image, produced by a script shipped in
  `scripts/`). Registration is admin API + config-as-code; UI can follow later.
- **Storage.** A dedicated hosted CRAN repo (e.g. `cran-linux`) holds binaries at
  `bin/linux/{distro}-{arch}/contrib/{rminor}/{pkg}_{ver}.tar.gz`, reusing
  `serveBinary`/`publishBin`/`binNS` (the platform segment may contain `/`, see
  `cranBinaryLocation`). Keeping binaries in their own repo means CRAN retention's
  "a version is source + all its binaries" rule (`retention.go`) cannot touch source.
- **Validation on PUT** (all static, table-driven tests for each rejection):
  - installed-package layout: `{pkg}/DESCRIPTION` with `Built:`, `Meta/package.rds`;
  - `Package`/`Version` match the path; `Built:` R minor and platform triple match
    the key;
  - matches the source: compare the source-only DESCRIPTION fields (`Packaged`,
    `Date/Publication`, `Repository` where present) against the source
    tarball for that version, fetched through the repo's configured
    `sourceRepo` (normally the group). No source available → reject;
  - every ELF object under `libs/`: `Machine` matches the arch; every `DT_NEEDED`
    is in the key's manifest; highest required `GLIBC_x.y` ≤ the key's glibc.
- **Write-once but evictable.** Overwriting an existing binary key → 409. Explicit
  removal and policy eviction remain allowed (see §6, open decision 1, for how this
  relates to the repo-level `Immutable` flag).
- `ledger.Record` on publish, `ledger.Forget` on delete. Trash case in
  `internal/cleanup/trash.go` covers the new tree.

### Phase L2 — Serving on one URL

- The group (e.g. `cran-public`) serves the PPM-style path from L0. For a request
  with a fully resolved key (distro from path; R minor + arch per L0):
  - `PACKAGES[.gz|.rds]` is the merged **source** index (so every package resolves),
    with entries that have a binary for that exact key adjusted per L0's findings;
  - a tarball GET returns the binary for the exact key if one exists, else the
    source tarball. Same for `Archive/` paths.
- One function decides "binary for this request, or source" and every read path
  calls it (principle 4).
- Responses on the binary path send `Vary: User-Agent` when the UA picks the key,
  so no intermediate cache serves one platform's binary to another.
- Unresolvable key (no UA R version, unregistered distro, arch mismatch) → source.
- Plain `src/contrib` on every repo is unchanged: source only, as today.

### Phase L3 — Provenance, system requirements, removal, retention

- Provenance record per binary, on the existing `{repo}:provenance` meta pattern
  (`internal/server/promote.go`): uploader identity, upload time, builder image
  digest (as reported by the uploader, via request header), source sha256,
  `LinkingTo` package versions read from the build's DESCRIPTION/installed deps
  (reported by the helper), and `DT_NEEDED` libraries (computed by forge).
- UI detail pane and API show provenance and "system requirements" (the needed
  libraries mapped to the manifest).
- **Remove binary**: one audited call (`DELETE` on the binary path, plus a button in
  the detail pane) that removes exactly that key; documented as the response to
  "binary fails to load".
- Retention: `LastDownloadedDays` on the binary repo evicts binaries not downloaded
  in N days (source untouched by construction, L1). Downloads served via the group
  must update the binary's last-download time, not only the source's — test this
  explicitly (one decision, many read paths).
- Webhook/audit event on upload, reject and removal.

### Phase L4 — Builder helper and conformance

- `scripts/cran-linux-upload.sh` (or an R script): run after `renv::restore()` /
  `install.packages()` in the builder; for each package in the library whose binary
  forge lacks for this key (HEAD first), pack the installed directory, PUT it with
  the builder token and image digest header. 409 = someone else won, ignore.
- Conformance test (Linux runner, real `rocker/r-ver:4.6` containers): cold source
  install → helper upload → fresh container gets binary, for `install.packages`,
  renv and pak; a package with compiled code (e.g. `jsonlite` or `data.table`) and
  one without.
- Negative conformance: other R minor, arm64 UA, other distro path, no UA → source.
  Bad uploads per L1 rejection list → 4xx with reason. Each guard mutation-tested.
- Extend `scripts/repo-kind-matrix.py` with the binary repo and group path.

### Phase L5 — Docs and seams

- `docs/setup.md`: register a platform, give the builder a grant, wire the helper
  into a build, point clients at the group's `__linux__/<distro>/` URL, remove a bad
  binary, configure eviction.
- CLAUDE.md format notes: CRAN serves Linux binaries via the group; seams checked
  (integrity verify covers the new tree; promotion and Nexus migration of Linux
  binaries are deliberate omissions unless trivially covered — record which).

---

## 4. Sequencing

```
L0  ███                     gate: path design depends on it
L1     ██████               (platform keys + validation; independent of serving)
L2        █████             needs L0 path decision, L1 storage
L3          █████           provenance can start with L1; removal/retention after L2
L4               ████       needs L1–L3
L5                  ██
```

Commit per self-contained unit (CLAUDE.md), not per phase.

---

## 5. Test strategy

```
Unit/handler  — every L1 rejection rule table-driven; ELF checks against small
                committed .so fixtures (built once for x86_64 and aarch64, with known
                DT_NEEDED and GLIBC versions); key-resolution function exhaustively.
Group serving — index merge + tarball choice per key; Vary header; last-download
                bookkeeping through the group.
Conformance   — real R/renv/pak in rocker containers, Linux CI runner (L4).
Live          — /format-live-test against a running forge before calling a phase done.
```

---

## 6. Open decisions (raise with the user before implementing that step)

1. **Write-once vs eviction.** Today `Immutable` repos refuse soft-delete and
   cleanup (`internal/cleanup/evict.go`). The binary repo needs "no overwrite, but
   eviction and removal allowed". Options: (a) a new repo setting alongside
   `Immutable`; (b) make the binary tree write-once in the CRAN handler regardless
   of repo flags. (b) is simpler but puts a storage rule in a format; (a) is
   reusable. Recommend (a); confirm first.
2. **Where R minor/arch come from** if L0 shows UA is unreliable for some client
   (path-only keys make lockfiles platform-specific; UA keys need `Vary`).
3. **Multiple builders per key.** Allowed if they share the registered image digest
   (self-reported, so this is advisory), or one builder identity per key?

---

## 7. Risks and accepted limits

| Risk | Mitigation / acceptance |
|------|------------------------|
| A compromised builder uploads a trojaned binary that passes every check | Accepted: only reproducible rebuilds could prove honesty, and R builds are not byte-reproducible. Mitigated by the dedicated grant, provenance, audit, and one-call removal. |
| Builder image digest is self-reported | Accepted; recorded for forensics, not trusted for security. The ELF manifest check is the enforced part. |
| A binary built against a `LinkingTo` package's headers breaks when that package's ABI changes (e.g. TMB vs Matrix) | Same limit as CRAN's own Windows/macOS binaries. `LinkingTo` versions recorded in provenance; removal fixes a broken one. |
| Mislabelled distro on a package that links nothing distro-specific | Undetectable, and harmless: such a binary loads on either release. |
| An HTTP cache between client and forge ignores `Vary` | Documented; prefer path-encoded keys where L0 shows clients support them. |
| First client per version still compiles | Inherent to build-once-on-demand; prewarming is a §8 follow-up. |
| A binary fails to load and renv does not fall back | One-call removal (L3), documented; next build recompiles. |

---

## 8. Out of scope / follow-ups

- **forge-side build service**: forge compiles source packages in a sandbox (no
  network, no secrets, resource-bounded, nothing shared between builds) and becomes
  another trusted uploader to the same tree. PPM parity; its own workplan.
- Prewarming: build a new version as soon as a proxy caches its source.
- Other distros (jammy, Debian, RHEL family) and arm64 — the key design supports
  them; only registration and conformance are needed.
- Signing binaries with a builder key (Nix-style) for verification outside forge;
  belongs with provenance (spine #9).
