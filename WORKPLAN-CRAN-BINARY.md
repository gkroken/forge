# forge — CRAN Binary Package Deliverable

> **Status: COMPLETE** — All phases B0–B3 shipped as of 2026-06-05.
> All DoD criteria in §1 are met. See commit history from `985727a` to `a0f391c`.

This plan takes the existing binary package handler (Windows `.zip` / macOS `.tgz`)
from "HTTP layer only, unproven with real clients" to a fully verified, operationally
complete feature.

---

## 0. Current state (at plan start)

- `bin/{platform}/contrib/{rver}/` routes exist for PUT, GET, and PACKAGES index
  (plain / `.gz` / `.rds`) in the hosted repo kind.
- Handler unit tests cover path parsing, publish, download, index generation, and
  platform isolation.
- Source conformance tests (hosted, pak, renv, proxy) exist and are green.
- **Nothing below was in place at plan start:**
  - `Built` and `Archs` PACKAGES fields are not emitted or stored.
  - No DELETE handler for binary packages.
  - Binary packages are invisible in `BrowseRepo` (UI browse shows source only).
  - No proxy or group support for binary trees.
  - No conformance test with a real R client performing an actual install.

---

## 1. Definition of Done

GA for the binary package surface is reached when **all** hold:

- A real R client (`install.packages`) successfully installs a binary package served
  by forge on the target platform (Windows via `.zip`; macOS via `.tgz`). Verified
  by CI conformance tests on platform-matched runners.
- `PACKAGES` / `PACKAGES.rds` include the `Built` and `Archs` fields when the
  uploaded package provides them. R's platform-compatibility filter can work
  correctly as a result.
- DELETE is available for binary packages with the same semantics as source delete.
- Binary packages appear in `BrowseRepo` (and therefore the UI browse page), grouped
  alongside source entries where applicable.
- A GET to `bin/{platform}/contrib/` returns the available R versions for that
  platform (platform enumeration).
- Proxy repositories can fetch and cache binary packages from an upstream CRAN mirror.
- Group repositories fan out binary requests across members.
- `go test ./...` green; conformance binary tests green on Windows and macOS runners.

---

## 2. Guiding principles

1. **No new deps.** Go stdlib only.
2. **Preserve, don't synthesise.** If `Built`/`Archs` are absent from the uploaded
   package's DESCRIPTION, serve them absent rather than fabricating values. R's
   fallback handles missing fields; fabricated wrong values cause silent breakage.
3. **Binary and source share code where possible.** `buildPackages`, `pkgRecord`,
   `buildPackagesRDS` are already shared; extend them rather than forking.
4. **Platform CI for binary conformance.** Linux containers cannot install Windows
   or macOS binary packages. Binary conformance tests run on `windows-latest` and
   `macos-latest` GitHub Actions runners and are gated in nightly CI.
5. **Handler tests first.** Conformance tests confirm the end-to-end story; handler
   tests pin each individual behaviour.

---

## 3. Phases

### Phase B0 — Field completeness  *(prerequisite for everything)*  ✅ `985727a`

The `pkgRecord` struct and the PACKAGES generator are missing the fields R uses for
binary compatibility filtering. Fix this before writing any conformance test, or the
test will succeed for the wrong reason (R skipping compatibility checks on absent
fields).

**Deliverables:**

- Add `Built`, `Archs`, and `OStype` fields to `pkgRecord`.
  - `Built` format: `R 4.4.0; x86_64-w64-mingw32; 2024-04-12 12:34:56 UTC; windows`
  - `Archs` format: `x64` or `i386, x64` (Windows multi-arch)
  - `OStype` format: `windows` or `unix` (used by `available.packages` filter)
- Update `parseBinDescription` / `parseDescriptionFromZip` to extract these three
  fields when present; leave them empty (NA in RDS) when absent.
- Update `buildPackages` to emit `Built`, `Archs`, `OStype` lines when non-empty.
  Source packages do not emit these fields; the binary code path already diverges,
  so no source-path regression risk.
- Update `buildPackagesRDS` column list to include `Built`, `Archs`, `OStype`.
- Update `makeWindowsBinPkg` (test helper) to include a realistic `Built` and
  `Archs` entry so all existing binary tests exercise the full field set.
- Add `makeMacOSBinPkg` (test helper) with a macOS `Built` line
  (`... ; aarch64-apple-darwin20; ...; unix`).
- Add golden files for binary `PACKAGES` and `PACKAGES.rds` output; use `-update`
  convention consistent with the existing golden framework.

**Exit:** `go test ./internal/format/cran/...` green; golden files lock the correct
field output; `buildPackages` round-trips Built/Archs/OStype for both Windows and
macOS fixtures.

---

### Phase B1 — Binary conformance tests  *(the gate)*  ✅ `b5df29a`

Without a real R client performing an actual install we have no proof the binary
surface works. These tests are the primary acceptance gate.

**Deliverables:**

- **`TestCRAN_Binary_Windows_PublishInstall`** — runs on `windows-latest` runner.
  Publishes a minimal pure-R Windows `.zip` binary to `cran-hosted` via PUT, then
  calls `install.packages(type="win.binary")` from R for Windows and verifies the
  package loads and executes.

- **`TestCRAN_Binary_macOS_PublishInstall`** — runs on `macos-latest` runner.
  Same flow with a `.tgz` binary and `type="mac.binary.big-sur-arm64"` (or
  `type="binary"` which R resolves per platform).

- **`TestCRAN_Binary_PACKAGES_Fields`** — runs on any Linux runner (no install
  needed). Publishes a binary with a known `Built`/`Archs` and asserts those fields
  appear verbatim in `PACKAGES`, `PACKAGES.gz`, and `PACKAGES.rds`. This guards
  against a regression where fields parse correctly but are silently dropped.

- **`TestCRAN_Binary_PlatformIsolation`** — publishes Windows and macOS binaries
  for the same package; asserts that `bin/windows/contrib/4.4/PACKAGES` does not
  list the macOS package and vice versa. (A handler test already covers this
  structurally; this conformance-level test confirms isolation at the index-parse
  layer.)

**CI wiring:**

- Windows and macOS conformance tests run in a new matrix job in the nightly
  workflow (`nightly.yml`); they do NOT run on every PR (platform runners are slow
  and expensive).
- The `TestCRAN_Binary_PACKAGES_Fields` and `TestCRAN_Binary_PlatformIsolation`
  tests run on Linux in the existing `conformance` CI job (they need no platform
  install).

**Exit:** both platform-install tests green in nightly CI; `PACKAGES_Fields` and
`PlatformIsolation` tests green on Linux; no regression in existing CRAN source
conformance tests.

---

### Phase B2 — Operational completeness  ✅ `3717b33`

Three gaps that block day-to-day admin use of binary repositories.

**B2a — DELETE for binary packages**

- Add a DELETE branch to `serveBinary` mirroring the source `deletePkg` logic:
  remove the blob at `c.Key(c.Sub)` and the meta record from `binNS(c, platform, rver)`.
- The meta key is `{Package}_{Version}` (same derivation as publish).
- Return 404 if blob absent; 204 on success.
- Guard with `c.Repo.Kind != repo.Hosted` check (same as PUT).
- Handler test: DELETE an existing binary → 204, then GET → 404, then PACKAGES
  no longer lists it.

**B2b — BrowseRepo includes binary packages**

- Extend `BrowseRepo` to merge binary entries alongside source entries.
- The merge key is `Package` name; a single `BrowseEntry` can include versions from
  source, Windows, and macOS trees. Add a `Platforms` field to `BrowseEntry` if the
  UI needs to distinguish them, otherwise deduplicate by `Package_Version` and let
  the detail page (per-component detail, WORKPLAN-UI §U2 gap #2) show platform
  breakdown.
- Handler test: upload a source package and a Windows binary of the same name;
  assert `BrowseRepo` returns one entry with both versions present.

**B2c — Platform enumeration**

- `GET bin/{platform}/contrib/` (path ends in `/`, no further segment) returns a
  plain-text or JSON list of R versions that have at least one binary package
  published. R clients that probe this path before selecting a version need a
  non-404 response.
- Implement by listing meta namespaces matching `{repo}:cran:bin:{platform}:*` and
  extracting the rver suffix. The FS meta store has no namespace-prefix scan; use
  the blob store to `List` blobs under `{repo}/bin/{platform}/contrib/` and
  extract version path segments.
- Return a simple newline-delimited list (mirrors CRAN's directory listing style).
- Handler test: upload packages for `4.3` and `4.4`; GET `bin/windows/contrib/` →
  response contains both versions.

**Exit:** DELETE handler test green; BrowseRepo test includes binary entries; platform
enumeration handler test green; no regression in existing CRAN tests.

---

### Phase B3 — Proxy and group support  *(last; depends on B0 and B1)*  ✅ `a0f391c`

Completes the feature parity gap. Proxy is the higher-value item (mirrors CRAN
binary trees for Windows/macOS users); group fan-out builds on it.

**B3a — Proxy binary trees**

- `serveBinary` currently rejects proxy repos at the PUT path but falls through to
  `http.Error("unsupported binary request", 404)` for GET requests rather than
  proxying. Fix the GET path in `downloadBin` to follow the same pattern as source
  `download`: check the blob cache first, then fetch from upstream and cache on miss.
- The upstream URL for a binary package on CRAN mirrors the sub-path exactly:
  `{upstream}/bin/{platform}/contrib/{rver}/{file}`. No path transformation needed.
- Wire proxy cache metrics (`CacheHits` / `CacheMisses`) the same way source proxy
  does.
- PACKAGES index for a proxy binary repo: generated from cached binaries only (same
  as source proxy behaviour — PACKAGES is not proxied verbatim because forge's index
  only lists what it has served and cached).
- Handler test: GET on a proxy repo for a binary that is not yet cached → triggers
  upstream fetch (mock upstream); subsequent GET → served from blob cache.
- Conformance test (nightly, Linux only — HTTP layer only): `TestCRAN_Binary_Proxy`
  — starts a minimal HTTP server serving a pre-built `.zip` fixture as the upstream;
  configures `cran-proxy` pointing at it; does a GET via forge; verifies cache-hit on
  second request.

**B3b — Group fan-out**

- `downloadBin` and `serveBinIndex` get group branches mirroring the source group
  logic in `groupDownload` and `groupPkgRecords`.
- For `serveBinIndex` in a group: merge binary records from all members for the
  requested platform+rver, deduplicating by `Package_Version` (first member wins).
- Handler test: group of two hosted repos each with a different binary package;
  group PACKAGES lists both; group download serves from the correct member.

**Exit:** proxy binary GET/cache test green; group binary PACKAGES merge test green;
nightly conformance proxy test green; no regression.

---

## 4. Gap → phase map (all resolved)

| Gap | Phase | Status |
|-----|-------|--------|
| `Built` / `Archs` / `OStype` not stored or emitted | **B0** | ✅ |
| `makeWindowsBinPkg` fixture missing Binary fields | **B0** | ✅ |
| `makeMacOSBinPkg` fixture does not exist | **B0** | ✅ |
| Golden files for binary PACKAGES / PACKAGES.rds | **B0** | ✅ |
| No real R install conformance test (Windows) | **B1** | ✅ |
| No real R install conformance test (macOS) | **B1** | ✅ |
| PACKAGES fields round-trip not verified end-to-end | **B1** | ✅ |
| No DELETE for binary packages | **B2a** | ✅ |
| Binary packages invisible in BrowseRepo / UI | **B2b** | ✅ |
| No platform/R-version enumeration endpoint | **B2c** | ✅ |
| No proxy support for binary trees | **B3a** | ✅ |
| No group fan-out for binary trees | **B3b** | ✅ |

---

## 5. Test strategy

```
Unit / handler   — cran_test.go — every new behaviour gets a table-driven test;
                   golden files lock PACKAGES / PACKAGES.rds output shape.

Conformance      — internal/conformance/cran_test.go
  Linux runners  — B1 PACKAGES_Fields, PlatformIsolation, B3a Proxy (HTTP only)
  Windows runner — B1 Windows install (install.packages + library load)
  macOS runner   — B1 macOS install (install.packages + library load)

CI cadence
  Per-PR          — unit + handler tests only (fast, platform-agnostic)
  Nightly         — adds Windows and macOS platform runners for B1 install tests
```

---

## 6. Sequencing

```
B0  ████
B1       ████    (needs B0 fixtures + fields to test correctly)
B2  ████████     (B2a/B2b/B2c are independent; can run alongside B0/B1)
B3           ████ (needs B0; B1 proxy conformance test lands here)
```

B0 and B2 can proceed in parallel. B1 blocks on B0 (fixture accuracy). B3 blocks on
B0 (field completeness applies to proxy-cached packages too).

---

## 7. Risks

| Risk | Mitigation |
|------|-----------|
| R version matrix changes binary compatibility semantics | Pin `r-base` and `rocker/r-ver` image tags in conformance tests; nightly smoke refreshes |
| macOS runner installs wrong R arch (arm64 vs x86_64) | Assert `R.version$arch` at test start; fail fast if wrong |
| `Built` field absent in real-world binary uploads | Preserve-not-synthesise policy (§2); R's fallback handles absent fields |
| Platform runner cost in CI | Limit to nightly; B1 Linux-only tests run on every PR as a cheap proxy signal |
| Proxy binary PACKAGES diverges from upstream | Document: forge's proxy PACKAGES is cache-derived; clients that need full upstream PACKAGES should configure upstream as a fallback |
