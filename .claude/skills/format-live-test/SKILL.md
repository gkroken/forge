---
name: format-live-test
description: How to test a forge repository format for real — drive a running server with real clients across hosted/proxy/group, write checks that can actually fail, and mutation-test every guard. Use when adding a format, changing one, hunting bugs in an existing one, or being asked to "test X properly".
---

# Testing a format against a live forge

Every serious bug found in this repo was found by **driving a running server**, not by
reading code. Handlers that were written, reviewed, unit-tested and shipped were serving
an empty repository for months. Reading confirms what the author intended; running is the
only thing that reports what a client actually gets.

Work in this order: **start a server → drive it with the real client → encode what you
learned into a script → mutate the guard to prove the check can fail.**

## 1. Start a disposable server

```bash
FT=${SCRATCH:-/tmp}/forge-livetest        # your session scratchpad if you have one
fuser -k 8099/tcp 2>/dev/null             # NEVER `pkill -f forge` — the pattern matches
                                          # your own command line and kills the session
rm -rf "$FT" && mkdir -p "$FT" && go build -o "$FT/forge" ./cmd/forge
("$FT/forge" -addr :8099 -data "$FT/data" >"$FT/log" 2>&1 &)
for i in $(seq 30); do curl -sf localhost:8099/healthz >/dev/null && break; sleep 0.3; done
```

Port 8099, not 8080 — `test.sh` owns 8080 and wipes `./data`. Add `-auth` when testing
anything about permissions; the bootstrap admin token is in the first-start log:

```bash
TOK=$(grep -o 'forge_[A-Za-z0-9_-]*' "$FT/log" | head -1)
```

A fresh data dir per run. State left over from the previous run is how a check passes for
the wrong reason.

## 2. Ask every question, for every repository kind

A format is three products. Most gaps live in the kinds nobody drives.

**Hosted** — publish, the index advertises it, download is byte-identical, delete, the
index stops advertising it, the artifact 404s. Then: republish over an immutable repo
(409), publish past a quota (507), publish something malformed (400, not a panic).

**Proxy** —
- the client-facing index is **upstream's**, not the (always empty) local records;
- links in that index point at **forge**, not upstream, or downloads bypass you entirely;
- an artifact the index names actually **downloads through forge**;
- a second read is served from cache — prove it by **counting upstream requests** or by
  ageing the cache entry past its TTL, *never* by comparing two response bodies;
- a package upstream does not have → **404**, not 502, and is negative-cached;
- an unreachable upstream → stale-on-error, not an error;
- every write verb → refused;
- browse/components lists **only what is cached**, not upstream's whole catalogue.

**Group** —
- a hosted member shadows a proxy member of the same name — including when the proxy is
  listed **first**;
- a claim shadows a proxy member for a name nothing has published yet;
- the download routes to whichever member holds it, through that member's own handler
  (so a proxy member fetches upstream rather than serving only what it cached);
- a dead member does not take the group down;
- every write verb → refused;
- a group containing a group, or itself, is refused rather than silently empty;
- a group is never more publicly readable than its least public member.

`scripts/repo-kind-matrix.py` is this checklist as code (all 6 formats × 3 kinds, live
upstreams). Run it, then **add your new questions to it** — a check you only ran by hand
is a check that decays.

## 3. Write checks that can fail

Every weak check found here asserted something *adjacent* to the property. Real examples,
all of which passed while the feature was completely broken:

| Check | Why it passed anyway |
|---|---|
| `GET /index.yaml → 200` | an empty index is a 200 — the proxy served no charts at all |
| "second read equals the first" | true for a cache that never expires **and** for a passthrough that doesn't exist |
| page contains "immutable" | matched the page chrome, not the outcome |
| RSS before vs after | the allocation spike is transient; sample *during* |
| a monitor grepping for success | silent through a crash — match the failure signatures too |

Assert the property itself: the index **lists** something, a listed artifact
**downloads**, the poisoned tag is **not served back**, the second request **did not reach
upstream**.

## 4. Use the real client, not your idea of it

Hand-rolled HTTP encodes your assumptions twice — in the server and in the test. Real
clients encode the ecosystem's. `helm repo add` + `helm search` + `helm pull`, `twine
upload` + `pip install`, `npm publish` + `npm install`, `docker pull`, R's
`install.packages`. `internal/conformance/` already drives clients in Docker:

```bash
go test -tags conformance -timeout 600s ./internal/conformance/...
```

Isolate CLI state so you test forge and not a warm cache:
`HELM_CACHE_HOME=… HELM_CONFIG_HOME=… HELM_DATA_HOME=…`, fresh `--userconfig`, etc.

## 5. Hunt the two shapes

Nearly every real bug here was one of these. Look for them directly:

**A rule written into a route instead of into the thing it guards.** Immutability enforced
in the repository route but not in `blob`; quota in `handleRepo` but not in the UI upload
or the migration; "only hosted repos accept writes" on each write route, so the endpoint
added later (npm dist-tags) had no guard at all; "a group is as public as its least public
member" in the admin API, so the browser form, GitOps apply, Nexus import and forge's own
seed all created the state the API refused. Fix by moving the rule into the thing that
owns the state, leaving a thin wrapper at the HTTP layer for the status code.

**One decision copied into N read paths, with a branch missing from some.** helm asked
"which records does this repository show" in `index()`, `listAll()`, `listOne()`,
`Inspect()` and `BrowseRepo()` — the proxy branch was missing from the first three, so
`helm search` found nothing while the UI looked fine. Grep for the decision, count the
copies, collapse them into one accessor.

## 6. Sweep the seams when the format is new or newly changed

The seams are methods on `format.Handler`, so the compiler forces an answer — but
`format.Unsupported` answers "no" for all of them, and "no" is silent. Walk the table in
`CLAUDE.md` and check what this format actually returns: an empty `OSVEcosystem()` means
no vulnerability scanning, ever, with nothing in the UI to say so (that is exactly how
CRAN went unscanned). Then walk the per-format switches outside `internal/format` —
trash, promote, browser upload, **and both** Nexus migration sites. A deliberate "no"
belongs in `internal/server/format_optout_test.go` so it reads as a decision.

## 7. Mutation-test every guard you add

A guard with a test that passes either way is decoration. Break the guard, confirm the
test fails, restore it:

```bash
cp internal/format/x/x.go /tmp/x.bak
# ...edit the guard out...
go build ./... || echo "INVALID MUTANT — a mutant that does not compile proves nothing"
go test -count=1 -run TestTheGuard ./internal/format/x/   # -count=1 or the cache lies
cp /tmp/x.bak internal/format/x/x.go
```

Two rules learned the hard way: a mutant that **fails to compile** is not a passing test,
and without `-count=1` Go serves a cached result from before the mutation. Mutate each
half of a compound rule separately — guarding proxies but forgetting groups is a real bug
shape, and only a per-half mutant catches it.

## 8. Close the loop

- Encode the new questions in `scripts/repo-kind-matrix.py` (behaviour per kind) or
  `scripts/security-probe.py` (guards; needs `FORGE_ADMIN_TOKEN` + `FORGE_BASE`).
- Add a conformance test in `internal/conformance/` if a real client is involved.
- Record the finding and the reasoning in `docs/notes/repo-kind-matrix.md`.
- Gates before pushing: `go test -race -count=1 ./...`, `gosec -severity medium
  -confidence medium ./...`, `bash test.sh`, the matrix, the probe.
- Never call a coverage gate from a local number — local reads ~1.3pp below CI.
