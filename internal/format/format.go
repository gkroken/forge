// Package format defines the plug-in contract for each package ecosystem.
//
// Adding a new ecosystem (Maven, npm, Helm, CRAN, Docker, ...) means writing
// one Handler. Everything else - storage, routing, repositories - is shared.
package format

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"forge/internal/blob"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/proxy"
	"forge/internal/queue"
	"forge/internal/repo"
)

// Context is everything a handler needs to serve one request.
type Context struct {
	Repo    repo.Repository // the resolved repository
	Blob    blob.Store      // raw bytes
	Meta    meta.Store      // structured metadata
	HTTP    *http.Client    // for proxy upstream fetches
	Sub     string          // request path *within* the repo (no leading slash)
	Repos   *repo.Manager   // non-nil; used by group handlers to look up members
	Queue   queue.Queue     // may be nil; if set, handlers enqueue async regen jobs
	Metrics *obs.Metrics    // may be nil; used to record per-repo cache counters

	// RepoStats is the per-repo hourly ring buffer (nil for non-proxy repos).
	RepoStats *obs.RepoStats
	// RepoStatsFn looks up per-repo stats by name; used by group handlers.
	RepoStatsFn func(string) *obs.RepoStats

	// GlobalStats accumulates server-wide request and cache metrics.
	GlobalStats *obs.GlobalStats
	// RetryGauge is a shared atomic counter of in-flight proxy retries.
	RetryGauge *atomic.Int32

	// OnCacheFill, if set, is called by the proxy after a cache miss fetched and
	// stored an artifact from upstream (blobKey = "{repo}/{sub}"). The server
	// wires it to emit an artifact.cached webhook event. May be nil.
	OnCacheFill func(blobKey string)

	// MemberFilter, if set, restricts which member repositories a group
	// fan-out may consult for the current request: MemberCtx returns
	// (nil, false) for members it rejects. The server's dependency-confusion
	// guard sets it on group contexts to exclude proxy members when the
	// requested component is claimed by — or actually present in — a hosted
	// member. Nil means all members are eligible.
	MemberFilter func(member repo.Repository) bool

	// NameClaimed, if set, reports whether a component name matches an
	// explicit ownership claim of this group's hosted members. Group index
	// merges (helm index.yaml, CRAN PACKAGES) consult it per record to drop
	// proxy-member entries for claimed names; unlike MemberFilter it is pure
	// (no storage I/O) so it is safe to call once per index entry. Non-nil
	// also signals that the dependency-confusion guard is active, which
	// enables hosted-name shadowing in those merges. Nil = guard inactive.
	NameClaimed func(name string) bool
}

// Key namespaces a blob key under the repo so repos never collide in storage.
func (c *Context) Key(sub string) string { return c.Repo.Name + "/" + sub }

// MemberCtx returns a sub-context for the named member repository.
// Returns (nil, false) if the member doesn't exist, is itself a group
// (groups cannot nest), or is rejected by MemberFilter (dependency-confusion
// guard: proxy members must not serve protected names).
func (c *Context) MemberCtx(name string) (*Context, bool) {
	if c.Repos == nil {
		return nil, false
	}
	r, ok := c.Repos.Get(name)
	if !ok || r.Kind == repo.Group {
		return nil, false
	}
	if c.MemberFilter != nil && !c.MemberFilter(r) {
		return nil, false
	}
	var memberStats *obs.RepoStats
	if c.RepoStatsFn != nil {
		memberStats = c.RepoStatsFn(r.Name)
	}
	return &Context{
		Repo: r, Blob: c.Blob, Meta: c.Meta, HTTP: c.HTTP, Sub: c.Sub,
		Repos: c.Repos, Queue: c.Queue, Metrics: c.Metrics,
		RepoStats: memberStats, RepoStatsFn: c.RepoStatsFn,
		GlobalStats: c.GlobalStats, RetryGauge: c.RetryGauge,
		OnCacheFill: c.OnCacheFill,
	}, true
}

// ProxyConfig builds a proxy.Config for this repo, wiring Prometheus counters
// (from Metrics), per-repo ring buffer (from RepoStats), global stats
// (from GlobalStats), and the shared retry gauge (from RetryGauge).
func (c *Context) ProxyConfig() proxy.Config {
	cfg := proxy.ConfigForRepo(c.Repo)
	cfg.RetryGauge = c.RetryGauge
	cfg.OnCacheFill = c.OnCacheFill

	if c.Metrics != nil {
		m, rname := c.Metrics, c.Repo.Name
		cfg.RecordHit = func() { m.CacheHits.WithLabelValues(rname).Inc() }
		cfg.RecordRevalidation = func() { m.CacheHits.WithLabelValues(rname).Inc() }
		cfg.RecordMiss = func() { m.CacheMisses.WithLabelValues(rname).Inc() }
	}
	if c.RepoStats != nil {
		s := c.RepoStats
		prevHit, prevReval, prevMiss := cfg.RecordHit, cfg.RecordRevalidation, cfg.RecordMiss
		cfg.RecordHit = func() {
			if prevHit != nil {
				prevHit()
			}
			s.RecordHit()
		}
		cfg.RecordRevalidation = func() {
			if prevReval != nil {
				prevReval()
			}
			s.RecordRevalidation()
		}
		cfg.RecordMiss = func() {
			if prevMiss != nil {
				prevMiss()
			}
			s.RecordMiss()
		}
		cfg.RecordNegative = s.RecordNegative
	}
	if c.GlobalStats != nil {
		gs := c.GlobalStats
		prevHit, prevMiss := cfg.RecordHit, cfg.RecordMiss
		cfg.RecordHit = func() {
			if prevHit != nil {
				prevHit()
			}
			gs.RecordCacheHit()
		}
		cfg.RecordMiss = func() {
			if prevMiss != nil {
				prevMiss()
			}
			gs.RecordCacheMiss()
		}
	}
	return cfg
}

// ErrNotSupported is returned by a seam a format deliberately does not
// implement. Callers map it to whatever "this format cannot do that" means at
// their layer — usually a 404 or 501, never a silent success.
var ErrNotSupported = errors.New("format: not supported by this format")

// Handler implements one package format.
//
// Every seam below is REQUIRED. That is the point: a format that does not
// answer one cannot be built, rather than silently lacking the feature. Nine
// of these used to be optional interfaces discovered by type assertion, and a
// format that skipped one compiled perfectly and quietly had no browse view, no
// vulnerability scanning, or no dependency-confusion protection — which is
// exactly how oci shipped with no retention.
//
// A format that genuinely does not need a seam embeds Unsupported and says so
// in one line. Adding a new seam here deliberately breaks every format until
// each one answers it.
type Handler interface {
	Format() string
	Serve(w http.ResponseWriter, r *http.Request, c *Context)

	// Browse and detail.
	BrowseRepo(c *Context) ([]BrowseEntry, error)
	Inspect(c *Context, baseURL, component string) (ComponentDetail, bool)

	// Vulnerability scanning. OSVEcosystem answers the capability question —
	// "can this format be scanned at all" — which a per-component call cannot,
	// since it has no component to be asked about. Empty means no OSV support,
	// and it is the same string OSVCoordinates returns, so the two cannot drift.
	OSVEcosystem() string
	OSVCoordinates(component string) (ecosystem, name string, ok bool)
	ReferencedImages(c *Context, component, version string) ([]string, error)
	VulnGateTarget(sub string) (component, version string, ok bool)

	// Dependency-confusion protection.
	ClaimPath(sub string) (component string, ok bool)
	OwnsComponent(c *Context, component string) bool

	// Integrity.
	VerifyIntegrity(c *Context, mode integrity.Mode) (integrity.Result, error)
	Reindex(ctx context.Context, c *Context) (int, error)
}

// Unsupported answers every optional seam with "not supported". Embed it in a
// Handler and override what the format actually does, so declining a seam is a
// visible line of code instead of an absence nobody notices.
//
// This is the grpc.UnimplementedFooServer pattern, which exists for this exact
// problem: methods silently missing from an implementation.
type Unsupported struct{}

func (Unsupported) BrowseRepo(*Context) ([]BrowseEntry, error) { return nil, ErrNotSupported }

func (Unsupported) Inspect(*Context, string, string) (ComponentDetail, bool) {
	return ComponentDetail{}, false
}

func (Unsupported) OSVEcosystem() string { return "" }

func (Unsupported) OSVCoordinates(string) (string, string, bool) { return "", "", false }

func (Unsupported) ReferencedImages(*Context, string, string) ([]string, error) {
	return nil, ErrNotSupported
}

func (Unsupported) VulnGateTarget(string) (string, string, bool) { return "", "", false }

func (Unsupported) ClaimPath(string) (string, bool) { return "", false }

func (Unsupported) OwnsComponent(*Context, string) bool { return false }

func (Unsupported) VerifyIntegrity(*Context, integrity.Mode) (integrity.Result, error) {
	return integrity.Result{}, ErrNotSupported
}

func (Unsupported) Reindex(context.Context, *Context) (int, error) { return 0, ErrNotSupported }

// BrowseEntry represents one component (package, chart, image, …) in a repo's
// browse view: a name and all known versions, newest-first where deterministic.
type BrowseEntry struct {
	Name      string
	Versions  []string
	UpdatedAt time.Time // zero if unknown
}

// Browsable is an optional extension to Handler that powers the web UI browse
// and search views. Handlers that do not implement it show a fallback message.

// ComponentDetail is the full metadata for one component, used by the detail page.
type ComponentDetail struct {
	Name           string
	Versions       []VersionInfo
	Description    string
	License        string
	Readme         string // plain text; may be empty
	Deps           []Dep
	InstallSnippet string // copy-pasteable install command(s)
}

// VersionInfo pairs a version string with its direct download URL (empty for OCI).
type VersionInfo struct {
	Version     string
	DownloadURL string
	PublishedAt time.Time // zero = unknown
	SizeBytes   int64     // 0 = unknown
	SHA256      string
	SHA1        string
	ContentType string // e.g. "application/java-archive"
	FileName    string // e.g. "spring-core-6.2.7.jar"
}

// Dep is one entry in a component's dependency list.
type Dep struct {
	Name       string
	Constraint string // e.g. ">= 1.0", may be empty
	SearchURL  string // /ui/search?q={name}
}

// Inspectable is an optional extension to Handler that powers the component
// detail page. baseURL is the scheme+host of the forge server (e.g.
// "http://localhost:8080"), used to build download URLs and install snippets.

// VulnCoordinates is an optional Handler extension that maps a forge component
// name to OSV's package vocabulary, so the vulnerability scanner can look up
// advisories for it. It mirrors the Inspectable idiom: format knowledge stays in
// the plugin, the scanner spine stays format-agnostic. ecosystem is OSV's
// ecosystem string (e.g. "npm", "Maven"); name is the OSV package name (which
// may differ from the forge component). Formats without a credible OSV source
// (helm, oci, cran) simply don't implement it and are skipped. The version is
// not needed to derive the coordinate, so callers pass it through separately.

// ReferencedImages is an optional Handler extension: a format whose stored
// components reference external container images (e.g. a Helm chart's values.yaml
// names the images its templates deploy) returns those refs so the vulnerability
// scanner can scan them too. The scanner stays format-agnostic — the parsing
// knowledge lives in the plugin, like VulnCoordinates. Refs are fully-qualified
// image references (e.g. "docker.io/nginx:1.19"); only helm implements it.

// VulnGate is an optional Handler extension used by the download-policy gate. It
// reverses a download sub-path back to the (component, version) the artifact
// belongs to, reporting ok=false for paths that are not primary artifacts
// (packuments, POMs, metadata, checksums, signatures) and therefore not subject
// to vulnerability enforcement. Only formats with a credible OSV source (npm,
// Maven) implement it; others are never gated. The returned component and
// version match the keys used by vuln.Store, so the gate looks findings up
// directly without re-deriving OSV coordinates.

// Claimable is an optional Handler extension powering dependency-confusion
// protection. It answers the two format-specific questions the guard needs;
// the enforcement policy itself lives in the server spine.
//
// ClaimPath reverses a request sub-path to the component path matched against
// ownership claims (internal/selector grammar): npm "@acme/foo/-/foo-1.0.tgz"
// → "@acme/foo", maven "com/acme/app/1.0/app-1.0.jar" → "com/acme/app", helm
// "mychart-1.2.3.tgz" → "mychart", cran "src/contrib/pkg_1.0.tar.gz" → "pkg".
// ok=false marks paths that do not address a single component (indexes,
// service endpoints) and are therefore never guarded.
//
// OwnsComponent reports whether the (hosted) repo in c contains any version
// of the component — the auto-derived ownership signal that protects a name
// in a group without an explicit claim. It must be cheap (one lookup, no
// upstream traffic); the caller memoizes per request.

// IntegrityChecker is an optional Handler extension that powers the read-only
// integrity verify job. The format knows what "consistent" means for its own
// storage shape (which meta records must be backed by blobs, where a checksum
// expectation is recorded), so that knowledge stays in the plugin and the
// verify spine stays format-agnostic — the Inspectable/VulnCoordinates idiom.
//
// Implementations must be strictly read-only: report findings, never repair.
// In integrity.ModeQuick no artifact bytes are hashed (small metadata
// documents may still be read); integrity.ModeFull re-verifies every stored
// checksum expectation against the bytes on disk.

// Reindexer is an optional Handler extension for formats that keep a
// materialized index which can be rebuilt from source records (npm's
// packument). Reindex rebuilds every such index in the repo and returns how
// many were rebuilt/enqueued. Formats that generate their indexes on demand
// (maven-metadata.xml, Helm index.yaml, CRAN PACKAGES) have nothing to
// rebuild and simply don't implement it.

// GroupBrowse merges BrowseRepo results from every member of a group context.
// First member that contains a given Name wins; output is sorted by Name.
func GroupBrowse(h Handler, c *Context) ([]BrowseEntry, error) {
	seen := map[string]struct{}{}
	var all []BrowseEntry
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		entries, _ := h.BrowseRepo(mc)
		for _, e := range entries {
			if _, exists := seen[e.Name]; !exists {
				seen[e.Name] = struct{}{}
				all = append(all, e)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// Registry maps a format name to its Handler.
type Registry struct{ byFormat map[string]Handler }

func NewRegistry() *Registry { return &Registry{byFormat: map[string]Handler{}} }

func (reg *Registry) Register(h Handler) { reg.byFormat[h.Format()] = h }

func (reg *Registry) For(format string) (Handler, bool) {
	h, ok := reg.byFormat[format]
	return h, ok
}
