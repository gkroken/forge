# forge — a multi-format package repository (prototype)

A working prototype of a Nexus-style artifact repository supporting **Maven,
npm, Helm, and CRAN**, in both **hosted** and **proxy** modes. Single static Go
binary, no external services required (filesystem storage + JSON metadata for
the prototype; both sit behind interfaces so S3/Postgres drop in for production).

## Why it's shaped this way

Every package format is a different protocol, but they all sit on the same spine.
The whole bet of this design is: **build the spine once, make formats a plugin.**

```
            HTTP request  /repository/{repo}/{path...}
                          │
                ┌─────────▼──────────┐
                │   server (router)  │   resolves repo name → Repository
                └─────────┬──────────┘
                          │ looks up Format → Handler
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   ┌────────┐        ┌────────┐        ┌────────┐
   │ maven  │        │  npm   │  ...   │  helm  │   format.Handler plugins
   └───┬────┘        └───┬────┘        └───┬────┘
       └─────────────────┼─────────────────┘
              ┌──────────┴──────────┐
              ▼                     ▼
        ┌──────────┐         ┌────────────┐
        │  blob    │         │   meta     │     storage interfaces
        │ (fs/S3)  │         │ (json/PG)  │
        └──────────┘         └────────────┘
```

Adding a format = implementing one interface:

```go
type Handler interface {
    Format() string
    Serve(w http.ResponseWriter, r *http.Request, c *Context)
}
```

Nothing in routing, storage, or the repository model knows what Maven is.

## Layout

```
cmd/forge/main.go          entrypoint: wires stores, registers handlers, defines repos
internal/blob/             blob.Store interface + filesystem impl (checksums, traversal-safe)
internal/meta/             meta.Store interface + JSON-file impl
internal/repo/             Repository model (hosted/proxy/group) + manager
internal/format/           Handler interface + registry
internal/format/maven/     Maven 2 layout: PUT/GET, checksum sidecars, maven-metadata.xml, proxy
internal/format/npm/       npm registry: publish, packument, tarballs, read-through proxy
internal/format/helm/      Helm repo: chart upload, index.yaml generation, chart API
internal/format/cran/      CRAN repo: DESCRIPTION parse, PACKAGES index, proxy
internal/server/           HTTP router and repo→handler dispatch
test.sh                    end-to-end smoke test (20 checks, all passing)
```

## Run it

```bash
go build -o forge ./cmd/forge
./forge -addr :8080 -data ./data
curl localhost:8080/            # lists configured repositories
```

Use it with real clients:

```bash
# npm — install through the proxy
npm install lodash --registry http://localhost:8080/repository/npm-proxy/

# Maven — point settings.xml / distributionManagement at:
#   http://localhost:8080/repository/maven-hosted/

# Helm
helm repo add forge http://localhost:8080/repository/helm-hosted/
helm push mychart-0.1.0.tgz   # (via helm-push plugin / ChartMuseum API)

# CRAN (in R)
# install.packages("pkg", repos="http://localhost:8080/repository/cran-hosted")
```

## Status matrix

| Format | Hosted | Proxy | Notes |
|--------|:------:|:-----:|-------|
| Maven  | ✅ | ✅ | generated maven-metadata.xml, synthesized md5/sha1/sha256 sidecars |
| npm    | ✅ | ✅ | publish + install; proxy rewrites tarball URLs; **verified with real npm CLI** |
| Helm   | ✅ | — | chart upload, index.yaml generation, chart list/delete API |
| CRAN   | ✅ | ✅ | DESCRIPTION parse, PACKAGES + PACKAGES.gz generation |

`test.sh` exercises all of the above (Maven/Helm/npm/CRAN hosted + a **live**
npm proxy fetch from registry.npmjs.org): 20/20 passing.

## Deliberately stubbed (the honest TODO list)

These are understood and scoped, just not built in the prototype:

- **Auth & RBAC** — no authentication yet. Production needs tokens, per-repo
  permissions, and OIDC/LDAP. Slots in as middleware before handler dispatch.
- **Group repositories** — the model has the `Group` kind; merging logic
  (e.g. unioning maven-metadata.xml / index.yaml across members) isn't written.
- **Maven SNAPSHOT** — timestamped snapshot metadata not handled; release
  versions work fully.
- **Maven parent-POM prefetch** in proxy mode (clients chase `<parent>` refs).
- **Helm OCI** (`helm push oci://…`) — needs a Docker/OCI registry handler.
- **CRAN PACKAGES.rds** — modern R reads PACKAGES for source installs; renv/pak
  prefer the R-serialized .rds, which needs an rds writer or an R process. Also
  no per-OS binary trees under /bin/.
- **Proxy cache policy** — currently cache-forever on read. Needs TTL, negative
  caching, and revalidation (ETag/Last-Modified).
- **Production stores** — swap blob.FS → S3 and meta.FS → Postgres (interfaces
  already in place). Add an index/search service for browse + search.
- **Admin API / UI** — repos are hardcoded in main.go; production needs CRUD.

## Suggested build order from here

1. Postgres-backed `meta.Store` + S3-backed `blob.Store` (prove the interfaces).
2. Auth middleware + token model + per-repo RBAC.
3. Proxy cache policy (TTL + revalidation) across all formats.
4. Group repositories + metadata merging.
5. Maven SNAPSHOT handling and Gradle `.module` metadata.
6. Helm OCI + a Docker/OCI registry handler (biggest new surface).
7. CRAN .rds + binary trees; search/index service; admin UI.

Realistic timeline to a serious all-four-formats OSS competitor: ~9–15 months
for a small team. This prototype is the spine that makes that tractable.

## DevOps adoption requirements

Three non-negotiable acceptance criteria for DevOps adoption are tracked in
`WORKPLAN.md` §1a and gated in CI (§5.12): **(A)** Kubernetes-native (Helm chart,
HA, probes/HPA/PDB), **(B)** Infrastructure as Code (Helm + Terraform, GitOps,
no click-ops), and **(C)** easy setup (`docker compose up` eval mode; < 10-minute
time-to-first-publish). The `blob.Store`/`meta.Store` interfaces already give the
zero-dependency eval mode and the Postgres+S3 production mode behind one codebase.
