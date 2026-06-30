// Package format defines the plug-in contract for each package ecosystem.
//
// Adding a new ecosystem (Maven, npm, Helm, CRAN, Docker, ...) means writing
// one Handler. Everything else - storage, routing, repositories - is shared.
package format

import (
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"forge/internal/blob"
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
}

// Key namespaces a blob key under the repo so repos never collide in storage.
func (c *Context) Key(sub string) string { return c.Repo.Name + "/" + sub }

// MemberCtx returns a sub-context for the named member repository.
// Returns (nil, false) if the member doesn't exist or is itself a group
// (groups cannot nest).
func (c *Context) MemberCtx(name string) (*Context, bool) {
	if c.Repos == nil {
		return nil, false
	}
	r, ok := c.Repos.Get(name)
	if !ok || r.Kind == repo.Group {
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

// Handler implements one package format.
type Handler interface {
	Format() string
	Serve(w http.ResponseWriter, r *http.Request, c *Context)
}

// BrowseEntry represents one component (package, chart, image, …) in a repo's
// browse view: a name and all known versions, newest-first where deterministic.
type BrowseEntry struct {
	Name      string
	Versions  []string
	UpdatedAt time.Time // zero if unknown
}

// Browsable is an optional extension to Handler that powers the web UI browse
// and search views. Handlers that do not implement it show a fallback message.
type Browsable interface {
	BrowseRepo(c *Context) ([]BrowseEntry, error)
}

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
type Inspectable interface {
	Inspect(c *Context, baseURL, component string) (ComponentDetail, bool)
}

// VulnCoordinates is an optional Handler extension that maps a forge component
// name to OSV's package vocabulary, so the vulnerability scanner can look up
// advisories for it. It mirrors the Inspectable idiom: format knowledge stays in
// the plugin, the scanner spine stays format-agnostic. ecosystem is OSV's
// ecosystem string (e.g. "npm", "Maven"); name is the OSV package name (which
// may differ from the forge component). Formats without a credible OSV source
// (helm, oci, cran) simply don't implement it and are skipped. The version is
// not needed to derive the coordinate, so callers pass it through separately.
type VulnCoordinates interface {
	OSVCoordinates(component string) (ecosystem, name string, ok bool)
}

// ReferencedImages is an optional Handler extension: a format whose stored
// components reference external container images (e.g. a Helm chart's values.yaml
// names the images its templates deploy) returns those refs so the vulnerability
// scanner can scan them too. The scanner stays format-agnostic — the parsing
// knowledge lives in the plugin, like VulnCoordinates. Refs are fully-qualified
// image references (e.g. "docker.io/nginx:1.19"); only helm implements it.
type ReferencedImages interface {
	ReferencedImages(c *Context, component, version string) ([]string, error)
}

// VulnGate is an optional Handler extension used by the download-policy gate. It
// reverses a download sub-path back to the (component, version) the artifact
// belongs to, reporting ok=false for paths that are not primary artifacts
// (packuments, POMs, metadata, checksums, signatures) and therefore not subject
// to vulnerability enforcement. Only formats with a credible OSV source (npm,
// Maven) implement it; others are never gated. The returned component and
// version match the keys used by vuln.Store, so the gate looks findings up
// directly without re-deriving OSV coordinates.
type VulnGate interface {
	VulnGateTarget(sub string) (component, version string, ok bool)
}

// GroupBrowse merges BrowseRepo results from every member of a group context.
// First member that contains a given Name wins; output is sorted by Name.
func GroupBrowse(h Browsable, c *Context) ([]BrowseEntry, error) {
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
