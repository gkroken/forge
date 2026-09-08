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
| P0 | Break the `format` → `cleanup` import cycle | prerequisite for P2 | ✅ done |
| P1 | Fold the 8 optional interfaces into `Handler` + `Unsupported` | **yes — ships alone** | ✅ done |
| P2 | Retention: 3 switch families → `ListVersions`/`DeleteVersion` | needs P0, P1 | ✅ done |
| P3 | Promote / migration / browser-upload switches | needs P1 | ✅ done (scoped) |
| P4 | Residual test for what no compiler can check | needs P1 | ✅ done |
| P5 | PyPI against the finished shape — the capstone proof | needs P1 (P2/P3 ideally) | — |

**P1 alone is most of the value.** It removes nine silent-absence seams, touches
no package boundaries, and can ship without ever starting P2. If this track gets
cut short, cut it after P1.

---

## Phase P0 — break the import cycle · ✅ COMPLETE

`internal/format/{maven,npm,helm,cran,oci}` all import `internal/cleanup` for
`RecordPublish` (added 2026-09-08 with the publish ledger). So `internal/cleanup`
can never import `internal/format`, which blocks P2's interface types.

- [x] Extract the publish ledger (`published.go`) into `internal/ledger`, importing
      only `internal/meta`. Both `format` and `cleanup` may then depend on it.
- [x] Call sites updated rather than re-exported — the diff was smaller, and a
      re-export would have left the misleading `cleanup.RecordPublish` name on a
      thing handlers call at publish time. Renamed while moving, since the package
      now carries the noun: `ledger.Record`/`RecordAt`/`Forget`/`Load`/`Resolve`/`Key`.
- [x] Acceptance: no package under `internal/format` imports `internal/cleanup`;
      `internal/ledger` depends only on `internal/meta`; `go build ./...`,
      `go vet ./...`, `go test ./...` and `bash test.sh` (20/20) all pass.

## Phase P1 — required interface + Unsupported default · ✅ COMPLETE

Acceptance: every optional interface is a `Handler` method; deleting a format's
method body breaks the build; `go vet ./...` clean.

- [x] Moved `Browsable`, `Inspectable`, `VulnCoordinates`, `ReferencedImages`,
      `VulnGate`, `Claimable`, `IntegrityChecker`, `Reindexer` into `Handler` and
      deleted the interface declarations.
- [x] Added `format.Unsupported` and `format.ErrNotSupported`.
- [x] **Added `OSVEcosystem() string`, which the plan did not anticipate.** Four
      call sites asked a *capability* question ("is this repo scannable at all?")
      that a per-component `OSVCoordinates(component)` cannot answer — there is no
      component to ask about. An ecosystem string answers it, and since it is the
      same string `OSVCoordinates` returns, the two cannot drift.
- [x] Embedded `Unsupported` in all five handlers — one line each, plus deleted
      `var _ format.X = (*Handler)(nil)` assertions replaced by a single
      `var _ format.Handler = (*Handler)(nil)`.
- [x] Replaced all 21 type assertions across 10 files with direct calls.
      **Not merely cosmetic:** once every handler satisfies every interface, the
      assertions all succeed, so sites that relied on `ok == false` for their
      "not supported" branch silently changed behaviour. Four tests caught it.
      Each site now keys off `ErrNotSupported`, an `ok` return, or
      `OSVEcosystem() != ""`.
- [x] Tests: `internal/format/unsupported_test.go` asserts a bare
      `struct{ format.Unsupported }` satisfies `Handler` and reports every seam as
      declined. A test stub in `webhooks_events_test.go` failed to compile until it
      embedded `Unsupported` — the mechanism working, on its first day.

## Phase P2 — retention through the interface · ✅ COMPLETE

The expensive one. ~900 lines of per-format enumeration currently live in
`internal/cleanup/{cleanup,dryrun,delete,trash}.go` (maven 149, oci 77, npm 55,
cran 42, helm 40, plus the dry-run/delete/trash equivalents). The generic policy
engine is only **64 lines** (`applyPolicies`) and stays put.

The split that makes this shrink rather than move: **formats enumerate, cleanup
decides.** Today each `runX` re-implements "list versions, apply rules, delete";
only the listing and deleting are format-specific.

- [x] Defined `format.Version{Component, Version, PublishedAt, BlobKeys}` plus
      `ListVersions`/`DeleteVersion`. Size and last-download time are derived
      generically from `BlobKeys`, and a zero `PublishedAt` is filled from the
      ledger — so a new format implements neither.
- [x] Implemented in all five formats as `internal/format/*/retention.go`.
- [x] Collapsed run, dry-run and delete into one generic path each.
      `internal/cleanup` went **4607 → 3781 lines** while the formats gained 454:
      a net reduction of ~370, as predicted.
- [x] **Trash left as a switch, deliberately.** Tombstone/restore is more than a
      delete — it captures meta for later restoration — and its `default` already
      errors loudly, so it is not a silent-absence seam. Recorded rather than
      forced.
- [x] OCI's scoped sweep moved intact into `internal/format/oci/retention.go`,
      now per-tag rather than per-run: after removing a tag it checks whether any
      other tag still points at that manifest before sweeping.
- [x] Acceptance: every cleanup test passes. Three were changed, each for a real
      reason rather than to fit the new shape — see the two bugs below.
- [x] **Bug found: CRAN retention never worked.** The handler writes records to
      `{repo}+cran`; `internal/cleanup` read `{repo}:cran` in four places (run,
      dry-run, delete, trash). The tests passed only because they seeded the
      namespace cleanup expected. Verified against a real publish. Dispatching
      through the handler fixes run/dry-run/delete; trash's literal was corrected
      in place.
- [x] **Inconsistency found: `Result.Deleted` counted different things.** Maven
      counted files, every other format counted versions. It is now uniformly
      versions, which is what the UI and history mean by "deleted N".

## Phase P3 — the remaining switches · ✅ COMPLETE (scoped)

**Finding that scoped this phase: all three remaining switches already fail
loudly.** Promote returns 501 "no promotion strategy for format X", migration
records "no transfer strategy for format X" in the job note, and browser upload
tells the user the format is unsupported. None of them is the silent-absence bug
this track exists to kill — that was cleanup's run/dry-run/delete returning a
zero value and a nil error, and those are gone.

What was worth removing is the *duplication*: promotion kept a second copy of
every format's storage layout in componentExists and componentBytes, which is
precisely the drift that made CRAN retention read the wrong namespace for its
entire life. Those now go through ListVersions.

The three strategy switches stay. They encode real per-format work — promotion
reconstructs a publish document and pushes it through the target's handler —
and converting them would move ~250 lines across five packages, against a
well-tested feature, to replace a loud failure with a compile-time one. Recorded
as a deliberate stop, not an oversight.

- [x] `internal/server/promote.go`: **15 case labels → 5.** componentExists and
      componentBytes were a second implementation of every format's layout; both
      now use `ListVersions`, with `sameComponent` reconciling maven's two
      spellings ("com.acme:app" vs "com/acme/app"). The 5 remaining labels are the
      copy strategies, kept deliberately.
- [x] `internal/server/migration_transfer.go`: left as a switch. Its default
      already records "no transfer strategy for format X" on the job, and the
      strategies need Nexus mapping knowledge that does not belong in a format
      package.
- [x] `internal/server/ui_upload.go`: left as a switch. It already tells the user
      "browser upload not supported for X"; converting it would buy nothing.

## Phase P4 — the residue · ✅ COMPLETE

Some things no compiler can check. One test, not a document:

- [x] `Registry.Formats() []string`, so the test walks the real registry rather
      than a hand-kept list that would drift from it.
- [x] `internal/cleanup/coverage_test.go`: a declared support matrix, checked
      against reality by probing each surface.
      - **Roll call** — a registered format missing from the table fails, so a new
        format cannot be added without stating what it supports. This is the half
        that matters; without it a new format is simply untested rather than red.
      - **Retention** probed through `cleanup.DryRun` (catches a format that
        embeds `Unsupported` and never overrides `ListVersions`).
      - **Trash** probed through `cleanup.TrashVersion`, the switch P2 left alone
        on purpose — nothing but a test can tell us a format was left out of it.
      - **browse.js** tree-vs-flat dispatch, the one seam that is not Go at all.
- [x] Mutation-tested all three guards: dropping `oci` from the table, claiming
      `oci` supports trash, and claiming `npm` renders as a tree each produce a
      failure naming the format and the fix. A guard that cannot fail is
      decoration.
- [x] Deliberately NOT covered: promote, migration and browser upload. They fail
      loudly at runtime already (P3), and a table that grows to cover everything
      becomes the hand-kept list this test exists to replace.

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

---

## Review findings (2026-09-08, after P0–P4)

A deliberate pass over the session's own work, looking for what would bite later.

**Fixed — data loss from a rule written on a false premise.** The publish ledger
refused to overwrite an existing entry, to stop "a proxy re-caching a tarball
making an old artifact look new". Proxies never write to the ledger; only the
five publish handlers do, so that rule protected against nothing. Meanwhile only
cleanup's two paths call `Forget`, so deleting a version through a format's own
API (npm unpublish, helm/maven/cran/oci DELETE — nine handlers) left the row
behind, and re-publishing the same coordinates inherited the old date. Retention
then deleted the brand-new artifact on its next run. Demonstrated with a test
before fixing; a publish now always stamps the time, which also makes a missed
`Forget` harmless rather than destructive.

**Fixed on review — the ledger grew with deletions.** This was first written
down as accepted, on the grounds that a leaked row is cosmetic once a publish
re-stamps. That undersold it: every row is read on every retention run, so
leaking them makes cleanup quietly slower forever, and the asymmetry was the
tell — five `Record` calls against two `Forget` calls. The native delete
handlers now forget: npm unpublish and tarball delete, helm delete, cran
deletePkg, oci deleteManifest. Maven only forgets when the last file under a
version goes, because a maven version is several files.
`TestNativeDelete_ForgetsLedgerEntry` drives the real routes and was
mutation-tested by removing npm's call.

**Accepted — `ledger.Load` is O(versions) per retention run.** It reads the whole
ledger into a map on every run. The per-format passes it replaced were also O(n)
over meta keys, so this is not a regression, but it is a second scan. Fine at
current scale; a Postgres-backed ledger would push the filter into the query.

**Accepted — `sameComponent` applies maven's colon rule to every format.** It
treats `a.b:c` and `a/b/c` as equal regardless of format. No other format's
component identity contains a colon (npm scopes use `@`, oci splits image from
tag before this point), so it is safe today, and it lives next to the promotion
code that needs it rather than in the formats.

**Accepted — `Result.Deleted` changed meaning for maven** (files → versions).
Historical cleanup-run records keep their old numbers, so a long history shows
both. Not worth a migration; the new meaning is the one the UI always implied.

**Known scaling limit — OCI's sweep is now per-tag rather than per-run.** Stated
as a neutral "note" first time round, which was too soft: this is a real cost P2
introduced, not a neutral observation. `DeleteVersion`
recomputes reachability for each tag it removes, where the old pass did it once.
Correctness is unchanged and it is simpler to reason about, but pruning many tags
from one image is O(tags x manifests). Revisit if OCI retention is ever used on a
registry with thousands of tags per image.

## Second review pass — hunting unchecked assumptions

The first two bugs this session both came from asserting a rationale without
checking it. So the second pass went looking specifically for claims made in
comments and commit messages that had never been verified.

**Fixed — an OCI dry run understated reclaimable space by orders of magnitude.**
`ListVersions` reported only the manifest as the version's blobs, and the generic
collector sums `BlobKeys` for size. A manifest is a couple of kilobytes in front
of layers that may be hundreds of megabytes, so a dry run answered "frees 2 KB"
before a run freed 500 MB — breaking the one question dry-run exists to answer.
`format.Version` now has an optional `SizeBytes` a format can set, and oci
reports its EXCLUSIVE size: the manifest plus blobs no other tagged manifest
references, computed from a single reference-count pass. Under-promising is the
safe direction, and two tests pin it, including the shared-layer case.

**Corrected — a misleading comment about vuln gating.** P1's rewrite claimed a
format with no OSV source answers `VulnGateTarget` false via `Unsupported`. oci
implements it deliberately: it is Trivy-gated, not OSV-gated. Behaviour was
unchanged either way — oci implemented `VulnGate` before P1 too — but the comment
would have misled the next reader into thinking gating implies OSV.
`TestUnsupportedDefaults_PreserveOptOut` now pins that gating and OSV mapping are
independent.

**Verified, held up:** dependency-confusion never guards oci (`ClaimPath` and
`OwnsComponent` both false); `cleanup.DeleteVersion` has no production callers;
trash, promote, migration and browser upload all fail loudly on an unknown
format; maven `DeleteVersion` accepts both component spellings; the S3 memory
bound was measured, not reasoned.

