# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o forge ./cmd/forge
./forge -addr :8080 -data ./data
```

Flags: `-addr` (listen address, default `:8080`), `-data` (data directory, default `./data`).

## Tests

```bash
# Unit tests
go test ./...

# Run a single package's tests
go test ./internal/blob/...
go test ./internal/meta/...

# End-to-end smoke test (starts server, exercises all formats, 20 checks)
go build -o forge ./cmd/forge && bash test.sh
```

`test.sh` requires the binary to be built first. It starts the server on `:8080`, clears `./data`, runs all format checks, and exits non-zero on failure. The npm proxy check makes a live call to `registry.npmjs.org` and is skipped (not failed) if upstream is unreachable.

## Key reference files

- `WORKPLAN.md` — the plan and acceptance criteria. §1 and §1a are the Definition of Done, including DevOps gates (Kubernetes-native, Infrastructure as Code, easy setup). A format isn't done until its conformance scenario there is green.
- `README.md` — architecture overview and client usage examples.

## Working conventions

- **Go standard library only.** Flag any new dependency before adding it.
- For changes over ~30 lines or spanning multiple files, state the plan and wait for confirmation before writing code.
- Never reprint a whole file. Show only changed functions or a diff.
- Keep `go test ./...` green at all times.

## Architecture

The design principle is: **one shared spine, formats are plugins.**

```
HTTP /repository/{repo-name}/{path...}
         │
    server.go         resolves repo name → Repository
         │             looks up Format → Handler
         ▼
  format.Registry     maps "maven"/"npm"/"helm"/"cran" → Handler
         │
  Handler.Serve()     receives format.Context (repo, blob, meta, http client, sub-path)
         │
  ┌──────┴──────┐
blob.Store   meta.Store    both are interfaces; only FS impls exist in the prototype
```

**Architecture invariants — do not violate:**
- Nothing in routing or storage knows about a specific format.
- Any new storage backend must satisfy the `blob.Store` / `meta.Store` interface contracts.
- Eval mode = filesystem blob + JSON meta (zero external deps). Production = S3 + Postgres behind the same interfaces.

**Key interfaces — these are the extension points:**

- `blob.Store` (`internal/blob/blob.go`): raw artifact bytes. Keys are `{repo-name}/{format-specific-path}`. FS impl in `fsstore.go`. Production target: S3.
- `meta.Store` (`internal/meta/meta.go`): namespaced JSON document store (npm packuments, Helm chart records, proxy cache). FS impl uses one `.json` file per record. Production target: Postgres.
- `format.Handler` (`internal/format/format.go`): one per ecosystem. `Format()` returns the string key; `Serve()` handles all HTTP for that format. Receives a `*format.Context` which provides both stores, the resolved repo, an HTTP client for proxy fetches, and the sub-path within the repo.

**Repository model** (`internal/repo/repo.go`): A `Repository` has `Name`, `Format`, `Kind` (`hosted`/`proxy`/`group`), and optional `Upstream` URL. Repositories are hardcoded in `cmd/forge/main.go`; production needs a DB + admin CRUD API. `group` kind is modeled but unimplemented.

**Adding a new format**: implement `format.Handler`, register it in `main.go` with `reg.Register(...)`, and add one or more `Repository` entries. No changes to routing, storage, or the repo model.

**URL routing**: `/repository/{repo-name}/{...rest}` — `server.go` strips the prefix, resolves the repo, dispatches to the handler. `format.Context.Sub` is the path after the repo name.

## Format implementations

Each lives in `internal/format/{name}/`:

- **Maven** (`maven.go`): PUT/GET using Maven 2 layout. Generates `maven-metadata.xml` aggregated over all versions present in the blob store. Synthesizes `.md5`/`.sha1`/`.sha256` sidecar responses on the fly from stored checksums. Proxy mode fetches from upstream and caches.
- **npm** (`npm.go`): Handles `PUT /{pkg}` (publish), `GET /{pkg}` (packument), `GET /{pkg}/-/{tarball}`. Proxy mode rewrites tarball URLs in packuments to point back at forge. Packuments stored in `meta.Store`; tarballs in `blob.Store`.
- **Helm** (`helm.go`): `POST /api/charts` (upload), `GET /index.yaml` (generated), `GET /{chart}.tgz` (download), `GET /api/charts` (list/delete API). Index generated from `meta.Store` records.
- **CRAN** (`cran.go`): `PUT /src/contrib/{pkg}_{ver}.tar.gz` (upload, parses DESCRIPTION), `GET /src/contrib/PACKAGES` and `PACKAGES.gz` (generated index). Proxy mode fetches from upstream CRAN.

## Data layout on disk

```
data/
  blobs/   blob.FS root — files stored at {repo-name}/{sub-path}
  meta/    meta.FS root — {ns}/{key}.json files (slashes in key → __)
```
