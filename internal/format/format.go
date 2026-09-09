// Package format defines the plug-in contract for each package ecosystem.
//
// Adding a new ecosystem (Maven, npm, Helm, CRAN, Docker, ...) means writing
// one Handler. Everything else - storage, routing, repositories - is shared.
package format

import (
	"context"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
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
// Key builds the blob-store key for a path inside this repository.
//
// The sub-path is cleaned so it can never climb out of the repository, because
// not every sub-path arrives from a URL. net/http cleans "../" out of request
// paths before routing, which protects handlers that key off the URL — but a
// handler keying off the request BODY (npm's attachment names) gets no such
// help, and one that did let a publisher write over another repository's
// artifacts. Cleaning here means no handler can make that mistake again.
func (c *Context) Key(sub string) string {
	if sub == "" {
		return c.Repo.Name + "/"
	}
	// Clean against "/" so any "..", however deep, resolves within the repo.
	cleaned := path.Clean("/" + sub)
	if strings.HasSuffix(sub, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/" // preserve a trailing slash: some keys are prefixes
	}
	return c.Repo.Name + cleaned
}

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
// ProxyMetadataConfig is ProxyConfig with the repository's MetadataMaxAge
// applied, for reads of an ecosystem's index rather than its artifacts.
//
// The two ages exist because they age differently: an artifact at a given
// coordinate never changes, while the index that lists it gains entries every
// time upstream publishes. A single TTL forces one to be wrong — either
// immutable content is re-validated needlessly, or new releases stay invisible.
//
// MetadataMaxAge was settable through the admin API and config for a long time
// while nothing read it: ConfigForRepo only ever consults ContentMaxAge, so the
// setting silently did nothing for every format.
func (c *Context) ProxyMetadataConfig() proxy.Config {
	cfg := c.ProxyConfig()
	if c.Repo.MetadataMaxAge != nil && *c.Repo.MetadataMaxAge > 0 {
		cfg.TTL = *c.Repo.MetadataMaxAge
	}
	return cfg
}

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

	// Browse and detail. BrowseAsTree says whether this format's storage has
	// meaningful folder hierarchy, so the browse UI shows a navigable tree
	// rather than a flat package list. It lives here because it is a fact about
	// the format's layout; it used to be a string comparison in browse.js, where
	// nothing but a test grepping the file could catch a new format being left
	// out of it.
	BrowseAsTree() bool
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

	// Retention. ListVersions enumerates what is stored; DeleteVersion removes
	// one. The policy itself — which versions are too old, too many, or unused —
	// is generic and lives in internal/cleanup, so a format only has to say what
	// exists and how to remove it.
	ListVersions(c *Context) ([]Version, error)
	DeleteVersion(c *Context, component, version string) (freedBytes int64, err error)
	// DeleteVersions removes several versions at once, for a format where
	// deleting one at a time repeats work — oci recomputes which blobs are still
	// reachable after every tag it drops. Answer ErrNotSupported (which
	// Unsupported does) and retention loops DeleteVersion instead, which is
	// right for a format whose versions own their bytes outright.
	//
	// An embedded Unsupported cannot call the outer type's DeleteVersion, so the
	// loop lives in the caller rather than in the default.
	DeleteVersions(c *Context, versions []Version) (freedBytes int64, err error)
}

// Version is one stored version of one component, as retention sees it.
//
// A format reports only what it alone knows. Size and last-download time are
// derived generically from BlobKeys, and a missing PublishedAt is filled from
// the publish ledger — so a new format implements neither.
type Version struct {
	Component string
	Version   string
	// PublishedAt is the format's own record of when this was published, if it
	// keeps one. Zero is normal and means "ask the ledger".
	PublishedAt time.Time
	// BlobKeys are the artifacts this version consists of. Retention reads their
	// download times, and sizes them when SizeBytes is zero; DeleteVersion is
	// what actually removes them, since some formats (oci) must do more than
	// delete these keys.
	BlobKeys []string
	// SizeBytes is what deleting this version would actually free. Leave it zero
	// and retention sums BlobKeys, which is right for a format whose version owns
	// its bytes outright. A format with shared storage must set it: an oci tag's
	// manifest is a couple of kilobytes in front of layers that may be hundreds
	// of megabytes, and layers shared with another tag are freed by neither.
	SizeBytes int64
}

// Unsupported answers every optional seam with "not supported". Embed it in a
// Handler and override what the format actually does, so declining a seam is a
// visible line of code instead of an absence nobody notices.
//
// This is the grpc.UnimplementedFooServer pattern, which exists for this exact
// problem: methods silently missing from an implementation.
type Unsupported struct{}

// BrowseAsTree defaults to a flat list, which is right for every format whose
// components are identified by name alone.
func (Unsupported) BrowseAsTree() bool { return false }

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

func (Unsupported) ListVersions(*Context) ([]Version, error) { return nil, ErrNotSupported }

func (Unsupported) DeleteVersion(*Context, string, string) (int64, error) {
	return 0, ErrNotSupported
}

func (Unsupported) DeleteVersions(*Context, []Version) (int64, error) {
	return 0, ErrNotSupported
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

// GroupFetch serves the first member of a group that answers successfully, and
// reports whether any did. Members are probed in configured order through a
// Capture, so a member's own handler decides what it has — a hosted member
// reads its blobs, a proxy member fetches and caches.
//
// TODO(refactor): maven.groupGet, npm.groupTarball, helm.groupDownload and
// cran.groupDownload/groupDownloadBin each hand-roll this exact loop. They
// predate this helper and are left alone deliberately: they carry real
// conformance coverage, and migrating them is a refactor of its own rather
// than a rider on a feature change. See docs/notes/group-generics.md.
func GroupFetch(h Handler, w http.ResponseWriter, r *http.Request, c *Context) bool {
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		cap := NewCapture()
		h.Serve(cap, r, mc)
		if cap.OK() {
			cap.Replay(w)
			return true
		}
	}
	return false
}

// GroupMerge applies a group repository's policy to whatever its members offer.
//
// The policy has no format in it, which is why this is generic over the record
// type: walk members in configured order, let the first member to offer a given
// component+version win, and drop anything a proxy member offers under a name
// the group already serves from a hosted member or that a claim covers. That
// last rule is the dependency-confusion protection — without it a public
// package can shadow an internal one of the same name.
//
// enumerate is supplied by the caller because "what does this member offer"
// genuinely differs: a hosted member reads its own records, while a proxy member
// must consult upstream (its local cache is only what has been downloaded so
// far, which would under-report what the group can actually serve).
//
// TODO(refactor): cran.mergeGroupRecords is this function written against
// cran's own record type, and npm/helm have narrower variants. They should
// collapse onto this once it has proven itself here.
func GroupMerge[T any](c *Context, enumerate func(*Context) []T, ident func(T) (component, version string)) []T {
	type memberResult struct {
		proxy bool
		items []T
	}
	var collected []memberResult
	hostedNames := map[string]bool{}

	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		items := enumerate(mc)
		isProxy := mc.Repo.Kind == repo.Proxy
		if !isProxy {
			for _, it := range items {
				comp, _ := ident(it)
				hostedNames[comp] = true
			}
		}
		collected = append(collected, memberResult{proxy: isProxy, items: items})
	}

	// Second pass, so a hosted member listed after a proxy still shadows it.
	seen := map[string]bool{}
	var out []T
	for _, m := range collected {
		for _, it := range m.items {
			comp, ver := ident(it)
			if m.proxy && (hostedNames[comp] || (c.NameClaimed != nil && c.NameClaimed(comp))) {
				continue
			}
			key := comp + "\x00" + ver
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, it)
		}
	}
	return out
}

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

// Formats lists every registered format key, sorted. It exists so a test can
// walk the real registry rather than a hand-kept list that drifts from it.
func (reg *Registry) Formats() []string {
	out := make([]string, 0, len(reg.byFormat))
	for f := range reg.byFormat {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (reg *Registry) For(format string) (Handler, bool) {
	h, ok := reg.byFormat[format]
	return h, ok
}
