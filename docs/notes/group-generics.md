# Group repositories: the generic core, and the four formats still hand-rolling it

**Status:** the generic helpers exist and PyPI uses them. Maven, npm, Helm and
CRAN still carry their own copies. This note records what the refactor is, why
it was not done at the same time, and what resists genericization.

## What is actually generic

A group repository does two things, and neither has any format in it:

**1. Download routing.** Walk the members in configured order, ask each one for
the path, serve the first success. This is `format.GroupFetch`, built on the
`format.Capture` response recorder that already existed for exactly this.

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

## The duplication to remove

| Site | Replace with |
|---|---|
| `maven.groupGet` member loop | `format.GroupFetch` |
| `npm.groupTarball` | `format.GroupFetch` |
| `helm.groupDownload` | `format.GroupFetch` |
| `cran.groupDownload`, `cran.groupDownloadBin` | `format.GroupFetch` |
| `cran.mergeGroupRecords` | `format.GroupMerge` + a CRAN renderer |
| `helm.groupRecords` | `format.GroupMerge` + a Helm renderer |
| `npm.groupPackument` | `format.GroupMerge` + dist-tag resolution kept in npm |

## Why it was deferred

These four formats have real conformance coverage, and `cran.mergeGroupRecords`
in particular has a subtlety worth preserving deliberately rather than by
accident: it gates hosted-name shadowing on `NameClaimed` being set, whereas
`format.GroupMerge` shadows unconditionally. That is a behaviour change, not a
move, and it belongs in a commit that says so — not as a rider on adding Python
support.

Do it as its own change, one format per commit, with the conformance suite green
between each.
