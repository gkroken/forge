# Group repositories: the generic core, and the four formats still hand-rolling it

**Status: done (2026-09-09).** Every format routes group downloads through
`format.GroupFetch`, and Helm and CRAN merge their indexes through
`format.GroupMerge`. What remains hand-rolled is listed at the end, with the
reason. This note records what the refactor was and what resists
genericization.

## What is actually generic

A group repository does two things, and neither has any format in it:

**1. Download routing.** Walk the members in configured order, ask each one for
the path, serve the first success. This is `format.GroupFetch`, built on
`format.Sink`, a response writer that forwards a member's response to the
client only once that member has answered successfully.

`Sink` replaced a recorder that buffered the whole response to decide whether
to replay it — a group serving a large artifact held all of it in memory, per
concurrent request, to answer a question the status line already answers. The
migration was worth doing for that alone.

**2. Index merging.** Merge what the members offer into one index, under one
policy: member order decides ties, a hosted member shadows a proxy member
offering the same name, a claim shadows a proxy member even when nothing is
published under that name yet, and identical entries are de-duplicated. This is
`format.GroupMerge`, generic over the record type.

The shadowing rules are the dependency-confusion protection. Without them a
public package can be served under the name of an internal one. `GroupMerge`
collects every member before deciding, so a hosted member shadows a proxy
listed *before* it — a single-pass merge silently loses that case, and
`TestGroup_HostedShadowsProxyListedFirst` pins it.

## What is NOT generic

Only two things, and they are why `GroupMerge` takes callbacks rather than
calling `ListVersions` itself:

- **Enumeration.** "What does this member offer?" differs by kind, not just by
  format: a hosted member reads its own records, but a proxy member must consult
  upstream. A proxy's local cache is only what has already been downloaded, so
  merging from it would hide versions the group can really serve.
- **Rendering.** Each ecosystem publishes its index in its own wire format:
  `maven-metadata.xml`, an npm packument, Helm's `index.yaml`, CRAN's DCF
  `PACKAGES`, PyPI's PEP 503 HTML.

Per-format policy that a generic merge cannot absorb:

| Format | Needs its own say |
|---|---|
| npm | dist-tags — which member's `latest` wins is policy, not merging |
| Maven | snapshot and version ordering inside `maven-metadata.xml` |
| CRAN | binary trees keyed by platform and R version |
| PyPI | none; the index is a flat list of links |

## The duplication that was removed

| Site | Now |
|---|---|
| `maven.groupGet` member loop | `format.GroupFetch` |
| `npm.groupTarball` | `format.GroupFetch` (function deleted) |
| `helm.groupDownload` | `format.GroupFetch` (function deleted) |
| `cran.groupDownload`, `cran.groupDownloadBin` | `format.GroupFetch` (both deleted) |
| `cran.mergeGroupRecords` | `format.GroupMerge` + the existing CRAN sort |
| `helm.groupRecords` | `format.GroupMerge` |

Each hand-rolled download loop also carried its own idea of what a proxy member
does. `npm.groupTarball` re-implemented the upstream fetch without npm's own
404-vs-unreachable distinction; `cran.groupDownload` and `groupDownloadBin`
called `HTTP.Get` directly and buffered the whole tarball in memory, bypassing
the shared fetcher's TTL, negative caching, circuit breaker and stale-on-error;
`helm.groupDownload` read only the member's blob store, so a proxy member in a
group could serve nothing it had not already cached. Routing through the
member's own handler is what makes those go away rather than needing five
fixes.

## Still hand-rolled, deliberately

`npm.groupPackument` merges packuments rather than a list of records: which
member's `latest` dist-tag wins is npm policy that a generic merge has no
opinion about, and the document it produces is not a set of (name, version)
pairs. `maven.groupMetadataBytes` merges the version list for one artifact
whose name is fixed by the request path, so it has no name to shadow on —
Maven's dependency-confusion protection filters the member itself, through
`MemberFilter`, before the merge is reached.

## Why it was deferred (history)

These four formats have real conformance coverage, so migrating them is its own
change rather than a rider on a feature.

One difference that used to block the migration is now gone. `cran` and `helm`
gated hosted-name shadowing on `NameClaimed` being set, whereas
`format.GroupMerge` shadows unconditionally — and that gate turned out to be a
bug rather than a policy: with the dependency-confusion guard switched off, a
CRAN group listed the same package twice, once from the hosted member and once
from upstream, which DCF cannot express. Both now separate the two rules the
way GroupMerge does: a hosted member always wins a name it holds (precedence),
and a claim additionally shadows names nothing has published yet (policy, and
the part the guard toggles). The semantics therefore match, and the migration is
now a move.

Do it as its own change, one format per commit, with the conformance suite green
between each.
