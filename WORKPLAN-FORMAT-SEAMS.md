# WORKPLAN — Format seams: make "formats are plugins" compiler-enforced

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done

CLAUDE.md claims a new format needs only `format.Handler` + `reg.Register` + repo
entries. It does not. A format has **sixteen** seams, and fourteen of them fail
**silently** when skipped — the feature is simply absent, no error, no test.
That is how `oci` shipped with no retention, and how npm's age rules stayed dead
long enough to be discovered by accident.

**Acceptance for the whole track: a new format that answers nothing must fail to
compile.** Not fail a test — fail the build. Adding a *new* seam later must break
every existing format until each one answers it, deliberately.

## The two failure modes, and why they are the same bug

| Mechanism | Seams | Skipping it gives you |
|---|---|---|
| Optional interface, runtime type assertion | 9 | feature silently absent |
| `switch format {}` outside `internal/format` | 7 | feature silently absent |

Converting switches to optional interfaces — the obvious move — fixes **nothing**:
a Go type assertion is as silent as a missing `case`. Both need the same answer.

## Target shape

The gRPC `UnimplementedFooServer` pattern, which exists for exactly this problem:

```go
type Handler interface {
	Format() string
	Serve(w http.ResponseWriter, r *http.Request, c *Context)
	BrowseRepo(c *Context) ([]BrowseEntry, error)
	ListVersions(c *Context) ([]Version, error)   // retention, promotion, migration
	DeleteVersion(c *Context, component, version string) error
	OSVCoordinates(component string) (ecosystem, name string, ok bool)
	// …every seam, required
}

// Unsupported answers every seam with ErrNotSupported. A format embeds it and
// overrides what it actually does, so "this format has no retention" is a
// deliberate, visible line of code rather than an absence nobody notices.
type Unsupported struct{}
func (Unsupported) ListVersions(*Context) ([]Version, error) { return nil, ErrNotSupported }
```

Callers stop type-asserting and start calling. `ErrNotSupported` surfaces as a
404/501 where a user can see it.

## Phase matrix

| Phase | Scope | Independent? | Risk |
|---|---|---|---|
| P0 | Break the `format` → `cleanup` import cycle | prerequisite for P2 | low |
| P1 | Fold the 9 optional interfaces into `Handler` + `Unsupported` | **yes — ships alone** | low |
| P2 | Retention: 4 switch families → `ListVersions`/`DeleteVersion` | needs P0, P1 | **high** |
| P3 | Promote / migration / browser-upload switches | needs P1 | medium |
| P4 | Residual test for what no compiler can check | needs P1 | low |
| P5 | PyPI against the finished shape — the capstone proof | needs P1 (P2/P3 ideally) | — |

**P1 alone is most of the value.** It removes nine silent-absence seams, touches
no package boundaries, and can ship without ever starting P2. If this track gets
cut short, cut it after P1.

---

## Phase P0 — break the import cycle

`internal/format/{maven,npm,helm,cran,oci}` all import `internal/cleanup` for
`RecordPublish` (added 2026-09-08 with the publish ledger). So `internal/cleanup`
can never import `internal/format`, which blocks P2's interface types.

- [ ] Extract the publish ledger (`published.go`) into `internal/ledger`, importing
      only `internal/meta`. Both `format` and `cleanup` may then depend on it.
- [ ] `internal/cleanup` re-exports the names it uses so call sites elsewhere are
      untouched, or update them — whichever diff is smaller.
- [ ] Acceptance: `go list -deps forge/internal/cleanup | grep forge/internal/format`
      is empty AND `go build ./...` passes.

## Phase P1 — required interface + Unsupported default

Acceptance: every optional interface is a `Handler` method; deleting a format's
method body breaks the build; `go vet ./...` clean.

- [ ] Move `Browsable`, `Inspectable`, `VulnCoordinates`, `ReferencedImages`,
      `VulnGate`, `Claimable`, `IntegrityChecker`, `Reindexer` (+ any found while
      working) into `Handler`.
- [ ] Add `format.Unsupported` with an `ErrNotSupported` answer for each, and
      `var ErrNotSupported = errors.New("format: not supported")`.
- [ ] Embed `Unsupported` in all five handlers; keep every existing method body.
      The diff per format should be one line plus deletions.
- [ ] Replace type assertions in `internal/server` with direct calls; map
      `ErrNotSupported` to the status each site already returns for "this format
      can't" (404/501), so behaviour is unchanged.
- [ ] Tests: a compile-time assertion per format (`var _ Handler = (*Handler)(nil)`),
      and one test that a bare `struct{ format.Unsupported }` returns
      `ErrNotSupported` from every seam.

## Phase P2 — retention through the interface

The expensive one. ~900 lines of per-format enumeration currently live in
`internal/cleanup/{cleanup,dryrun,delete,trash}.go` (maven 149, oci 77, npm 55,
cran 42, helm 40, plus the dry-run/delete/trash equivalents). The generic policy
engine is only **64 lines** (`applyPolicies`) and stays put.

The split that makes this shrink rather than move: **formats enumerate, cleanup
decides.** Today each `runX` re-implements "list versions, apply rules, delete";
only the listing and deleting are format-specific.

- [ ] Define `format.Version{Component, Version string; SizeBytes int64;
      PublishedAt time.Time; DownloadedAt time.Time}` and the two methods.
- [ ] Implement `ListVersions`/`DeleteVersion` in each format, moving the bodies
      out of `internal/cleanup`.
- [ ] Collapse `run{Maven,NPM,Helm,CRAN,OCI}` into one generic runner; same for
      dry-run, delete and trash. Expect a net **reduction** in total lines.
- [ ] Keep OCI's scoped sweep as `DeleteVersion`'s own business — it is genuinely
      format-specific (see `internal/cleanup/oci.go` on why the sweep is narrow).
- [ ] Acceptance: every existing cleanup test passes untouched. They are the
      contract; do not rewrite them to fit the new shape.

## Phase P3 — the remaining switches

- [ ] `internal/server/promote.go` (517 lines, 15 case labels) — the messiest.
      Promotion is copy-through-the-handler already; it should collapse hard.
- [ ] `internal/server/migration_transfer.go` (470 lines) — needs Nexus mapping
      knowledge; consider leaving as a switch with a `default` that errors, and
      record that as a deliberate exception.
- [ ] `internal/server/ui_upload.go` (236 lines) — already fails loudly; convert
      for consistency, low value on its own.

## Phase P4 — the residue

Some things no compiler can check. One test, not a document:

- [ ] `Registry.Formats() []string` (does not exist yet — 3 lines).
- [ ] A test asserting every registered format has a `main.go` repository entry
      and appears in `browse.js`'s tree-vs-flat dispatch, or is explicitly listed
      as not needing one.

## Phase P5 — PyPI, as the capstone was meant to be

- [ ] Build `internal/format/pypi/` against the finished shape: PEP 503 simple
      index, twine upload, wheel/sdist filename parsing, PEP 503 name
      normalization (`Foo.Bar` == `foo-bar` == `foo_bar` — component identity,
      claims and the publish ledger must all agree on the normalized form),
      proxy mode against pypi.org, `OSVCoordinates` → `"PyPI"`.
- [ ] Acceptance, and the whole point: the diff touches `internal/format/pypi/`,
      `reg.Register`, and `main.go` repo entries — **and nothing else**. If it
      touches anything else, that is a seam P1–P3 missed, and it goes on this list
      rather than being patched around.

---

## Honest sizing

P1 is a day-shaped change. P2 is the real work and touches five format packages
plus four files in `internal/cleanup`; it is worth doing only because the tests
already exist to prove it. P3 is optional. **Stopping after P1+P0 leaves the
codebase strictly better than it is now** and PyPI still lands against enforced
interface seams.

## Rejected alternatives

- **A completeness test instead of the interface.** Detects the error rather than
  forbidding it, and a test can be skipped, deleted, or lag a newly added seam.
  Kept only for P4, where no compiler can help.
- **Converting switches to optional interfaces.** Moves the bug: a runtime type
  assertion is exactly as silent as a missing `case`.
- **One giant interface with no default.** Would force every format to write
  fourteen stub methods, and the stubs would rot. `Unsupported` makes declining a
  seam one line.
