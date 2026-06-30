package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/format"
	"forge/internal/repo"
	"forge/internal/vuln"
)

// uiBrowseTree serves GET /ui/browse/{repo}/tree?prefix=
//
// Returns a JSON array of tree nodes directly under the given prefix, one
// level deep. Prefix is relative to the repo root (no leading slash).
//
//	[{"name":"com","path":"com","is_dir":true}, ...]
func (s *Server) uiBrowseTree(w http.ResponseWriter, r *http.Request, repoName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		jsonError(w, "repository not found: "+repoName, http.StatusNotFound)
		return
	}

	prefix := strings.Trim(r.URL.Query().Get("prefix"), "/")

	// A group owns no blobs of its own — its artifacts live under the member
	// repos. Fan out across members so a group browses the merged tree; a
	// hosted/proxy repo just scans itself.
	sources := []string{rp.Name}
	if rp.Kind == repo.Group && len(rp.Members) > 0 {
		sources = rp.Members
	}

	// Collect immediate children (one level deep), classifying each as a folder
	// (groupId segment, keep descending) or an artifact (its children are
	// versions, so the tree terminates here). A child is an artifact when any of
	// its grandchild segments starts with a digit — the same version heuristic
	// the maven handler uses. Versions never become tree nodes; they live in the
	// center pane, reached by clicking the artifact's emitted Component.
	type childInfo struct {
		isDir      bool
		hasVersion bool
	}
	seen := map[string]*childInfo{}
	var order []string
	for _, src := range sources {
		blobPrefix := src + "/"
		if prefix != "" {
			blobPrefix += prefix + "/"
		}
		keys, err := s.Blob.List(blobPrefix)
		if err != nil {
			continue // skip a member that fails to list; merge the rest
		}
		for _, k := range keys {
			rel := strings.TrimPrefix(k, blobPrefix)
			if rel == "" {
				continue
			}
			seg, rest, isDir := strings.Cut(rel, "/")
			if seg == "" {
				continue
			}
			ci := seen[seg]
			if ci == nil {
				ci = &childInfo{}
				seen[seg] = ci
				order = append(order, seg)
			}
			if isDir {
				ci.isDir = true
				if g, _, _ := strings.Cut(rest, "/"); g != "" && g[0] >= '0' && g[0] <= '9' {
					ci.hasVersion = true
				}
			}
		}
	}

	// One rollup per source (covers groups, whose findings live under the member
	// repos, not the group). Looked up once per tree level, not per node.
	rollups := make([]vuln.Rollup, len(sources))
	for i, src := range sources {
		rollups[i] = s.vulnRollupFor(src)
	}

	nodes := make([]treeNode, 0, len(order))
	for _, seg := range order {
		ci := seen[seg]
		nodePath := seg
		if prefix != "" {
			nodePath = prefix + "/" + seg
		}
		n := treeNode{Name: seg, Path: nodePath}
		switch {
		case ci.hasVersion:
			n.Component = mavenComponent(prefix, seg)
			for _, rr := range rollups {
				if sev := rr.ComponentSeverity(n.Component); sev != "" {
					n.Severity = sev
					break
				}
			}
		case ci.isDir:
			n.IsDir = true
		}
		nodes = append(nodes, n)
	}
	writeJSON(w, nodes)
}

// handleComponents serves GET /api/v1/repos/{name}/components
//
//	?q=      substring filter on component name (case-insensitive)
//	?page=   1-based page number (default 1)
//	?limit=  page size (default 50, max 5000); 0 or "all" returns every
//	         component in one response (the UI list views rely on this to show
//	         the full cached set of large proxy repos and filter client-side).
func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rp, ok := s.Repos.Get(name)
	if !ok {
		jsonError(w, "repository not found: "+name, http.StatusNotFound)
		return
	}

	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		jsonError(w, "no handler for format: "+rp.Format, http.StatusNotImplemented)
		return
	}

	page := clampedInt(r, "page", 1, 1, 1<<20)
	// limit=0 or "all" → unbounded (return every component); otherwise clamp.
	limit := 50
	unbounded := false
	if lp := r.URL.Query().Get("limit"); lp == "all" {
		unbounded = true
	} else if lp != "" {
		if v, err := strconv.Atoi(lp); err == nil {
			switch {
			case v <= 0:
				unbounded = true
			case v > 5000:
				limit = 5000
			default:
				limit = v
			}
		}
	}
	q := strings.ToLower(r.URL.Query().Get("q"))

	b, ok := h.(format.Browsable)
	if !ok {
		writeJSON(w, componentsResponse{Components: []componentItem{}, Total: 0, Page: page, Limit: limit})
		return
	}

	c := &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
		Repos: s.Repos, Metrics: s.Metrics,
	}
	entries, err := b.BrowseRepo(c)
	if err != nil {
		jsonError(w, "browse failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if q != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Name), q) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	total := len(entries)
	if unbounded {
		// Return the full set; page/limit collapse to one page covering everything.
		page = 1
		limit = total
	} else {
		start := (page - 1) * limit
		if start < total {
			end := start + limit
			if end > total {
				end = total
			}
			entries = entries[start:end]
		} else {
			entries = nil
		}
	}

	rollup := s.vulnRollupFor(name)
	items := make([]componentItem, len(entries))
	for i, e := range entries {
		items[i] = componentItem{
			Name:      e.Name,
			Versions:  e.Versions,
			UpdatedAt: e.UpdatedAt,
			Severity:  rollup.ComponentSeverity(e.Name),
		}
	}
	writeJSON(w, componentsResponse{Components: items, Total: total, Page: page, Limit: limit})
}

// handleSearch serves GET /api/v1/search
//
//	?q=       required; substring match against component names
//	?format=  optional; filter to one format (maven, npm, helm, cran, oci)
//	?repo=    optional; filter to one repository
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	filterFormat := r.URL.Query().Get("format")
	filterRepo := r.URL.Query().Get("repo")

	if strings.TrimSpace(q) == "" {
		writeJSON(w, searchResponse{Query: q, Results: []searchResult{}})
		return
	}

	ql := strings.ToLower(q)
	var results []searchResult

	for _, rp := range s.Repos.All() {
		if filterFormat != "" && rp.Format != filterFormat {
			continue
		}
		if filterRepo != "" && rp.Name != filterRepo {
			continue
		}
		h, ok := s.Handlers.For(rp.Format)
		if !ok {
			continue
		}
		b, ok := h.(format.Browsable)
		if !ok {
			continue
		}
		c := &format.Context{
			Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
			Repos: s.Repos, Metrics: s.Metrics,
		}
		entries, err := b.BrowseRepo(c)
		if err != nil {
			continue
		}
		rollup := s.vulnRollupFor(rp.Name) // one O(1) lookup per repo, not per hit
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Name), ql) {
				results = append(results, searchResult{
					Repo:     rp.Name,
					Format:   rp.Format,
					Name:     e.Name,
					Versions: e.Versions,
					Severity: rollup.ComponentSeverity(e.Name),
				})
			}
		}
	}

	if results == nil {
		results = []searchResult{}
	}
	writeJSON(w, searchResponse{Query: q, Results: results})
}

// uiBrowseVersions serves GET /ui/browse/{repo}/versions?pkg=
// Returns a JSON list of versions for the given package using Inspect or BrowseRepo.
func (s *Server) uiBrowseVersions(w http.ResponseWriter, r *http.Request, repoName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		jsonError(w, "repository not found: "+repoName, http.StatusNotFound)
		return
	}
	pkg := r.URL.Query().Get("pkg")
	if pkg == "" {
		jsonError(w, "pkg is required", http.StatusBadRequest)
		return
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		jsonError(w, "no handler for format", http.StatusNotImplemented)
		return
	}

	c := s.browseCtx(rp)
	rollup := s.vulnRollupFor(repoName)
	resp := browseVersionsResponse{Name: pkg, Pkg: pkg}

	if insp, ok := h.(format.Inspectable); ok {
		detail, found := insp.Inspect(c, publicBase(r), pkg)
		if !found {
			jsonError(w, "component not found", http.StatusNotFound)
			return
		}
		for _, v := range detail.Versions {
			resp.Versions = append(resp.Versions, browseVersionRow{
				Version:     v.Version,
				PublishedAt: v.PublishedAt,
				SizeBytes:   v.SizeBytes,
				DownloadURL: v.DownloadURL,
				Severity:    rollup.VersionSeverity(pkg, v.Version),
			})
		}
	} else if b, ok := h.(format.Browsable); ok {
		entries, err := b.BrowseRepo(c)
		if err != nil {
			jsonError(w, "browse failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		for _, e := range entries {
			if e.Name == pkg {
				for _, v := range e.Versions {
					resp.Versions = append(resp.Versions, browseVersionRow{
						Version:  v,
						Severity: rollup.VersionSeverity(pkg, v),
					})
				}
				break
			}
		}
	}
	if resp.Versions == nil {
		resp.Versions = []browseVersionRow{}
	}
	writeJSON(w, resp)
}

// uiBrowseDetail serves GET /ui/browse/{repo}/detail?pkg=&ver=
// Returns a JSON object with asset metadata for the given package version.
func (s *Server) uiBrowseDetail(w http.ResponseWriter, r *http.Request, repoName string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		jsonError(w, "repository not found: "+repoName, http.StatusNotFound)
		return
	}
	pkg := r.URL.Query().Get("pkg")
	ver := r.URL.Query().Get("ver")
	if pkg == "" || ver == "" {
		jsonError(w, "pkg and ver are required", http.StatusBadRequest)
		return
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		jsonError(w, "no handler for format", http.StatusNotImplemented)
		return
	}
	insp, ok := h.(format.Inspectable)
	if !ok {
		jsonError(w, "format does not support inspection", http.StatusNotImplemented)
		return
	}

	c := s.browseCtx(rp)
	detail, found := insp.Inspect(c, publicBase(r), pkg)
	if !found {
		jsonError(w, "component not found", http.StatusNotFound)
		return
	}

	blobStore := rp.BlobStore
	if blobStore == "" {
		blobStore = "default"
	}
	resp := browseDetailResponse{
		Name:      pkg,
		Version:   ver,
		Format:    rp.Format,
		Repo:      repoName,
		BlobStore: blobStore,
		IsProxy:   rp.Kind == repo.Proxy,
	}
	for _, v := range detail.Versions {
		if v.Version == ver {
			resp.PublishedAt = v.PublishedAt
			resp.DownloadURL = v.DownloadURL
			resp.SizeBytes = v.SizeBytes
			resp.SHA256 = v.SHA256
			resp.SHA1 = v.SHA1
			resp.ContentType = v.ContentType
			resp.FileName = v.FileName
			break
		}
	}
	if resp.DownloadURL == "" {
		resp.DownloadURL = publicBase(r) + "/repository/" + repoName + "/" + pkg
	}
	resp.Vuln = s.vulnInfoFor(rp, h, pkg, ver)
	writeJSON(w, resp)
}

// vulnRollupFor returns the persisted rollup for repo, or a zero rollup when
// scanning is unconfigured or the repo was never scanned. The zero value is
// safe to query (ComponentSeverity/VersionSeverity return "" → no badge), so
// list surfaces can read severity unconditionally with a single O(1) lookup.
func (s *Server) vulnRollupFor(repo string) vuln.Rollup {
	if s.Vuln == nil {
		return vuln.Rollup{}
	}
	r, ok, err := s.Vuln.GetRollup(repo)
	if err != nil || !ok {
		return vuln.Rollup{}
	}
	return r
}

// vulnInfoFor builds the security panel data for one component@version. nil when
// scanning isn't configured. Distinguishes four states the UI renders
// differently: unsupported format, supported-but-unscanned, scanned-and-clean,
// and scanned-with-advisories.
func (s *Server) vulnInfoFor(rp repo.Repository, h format.Handler, pkg, ver string) *vulnInfo {
	if s.Vuln == nil {
		return nil
	}
	vi := &vulnInfo{}
	// A version is "scannable" if any producer covers its format: OSV (formats
	// implementing VulnCoordinates — npm/Maven) or the Trivy sidecar (OCI, when
	// configured). Keying solely on VulnCoordinates wrongly reported OCI images
	// as unsupported even after Trivy had written a finding for them.
	_, osvSupported := h.(format.VulnCoordinates)
	vi.Supported = osvSupported || s.trivyScannable(rp.Format)
	if !vi.Supported {
		return vi // e.g. helm/cran: "not scanned — unsupported"
	}
	f, ok, _ := s.Vuln.Get(rp.Name, pkg, ver)
	if !ok {
		return vi // supported but no finding yet: "not yet scanned"
	}
	vi.Scanned = true
	vi.ScannedAt = f.ScannedAt
	if len(f.Advisories) > 0 {
		vi.Severity = f.Worst().String()
	}
	for _, a := range f.Advisories {
		vi.Advisories = append(vi.Advisories, vulnAdvisoryDTO{
			ID:       a.ID,
			Severity: a.Severity.String(),
			Summary:  a.Summary,
			URL:      a.URL,
			FixedIn:  a.FixedIn,
			Aliases:  a.Aliases,
		})
	}
	return vi
}

// ── types ─────────────────────────────────────────────────────────────────────

type browseVersionsResponse struct {
	Name     string             `json:"name"`
	Pkg      string             `json:"pkg"`
	Versions []browseVersionRow `json:"versions"`
}

type browseVersionRow struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	Severity    string    `json:"severity,omitempty"` // worst severity for this version; "" = not vulnerable
}

type browseDetailResponse struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Format      string    `json:"format"`
	Repo        string    `json:"repo"`
	BlobStore   string    `json:"blob_store,omitempty"`
	IsProxy     bool      `json:"is_proxy"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	DownloadURL string    `json:"download_url"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	SHA1        string    `json:"sha1,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	FileName    string    `json:"file_name,omitempty"`
	Vuln        *vulnInfo `json:"vuln,omitempty"`
}

// vulnInfo is the security panel payload for a component version.
type vulnInfo struct {
	Supported  bool              `json:"supported"`            // format has an OSV coordinate mapping
	Scanned    bool              `json:"scanned"`              // a finding exists for this version
	Severity   string            `json:"severity,omitempty"`   // worst advisory severity (omitted when clean)
	ScannedAt  time.Time         `json:"scanned_at,omitempty"` //
	Advisories []vulnAdvisoryDTO `json:"advisories,omitempty"`
}

type vulnAdvisoryDTO struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Summary  string   `json:"summary,omitempty"`
	URL      string   `json:"url,omitempty"`
	FixedIn  []string `json:"fixed_in,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
}

// treeNode is one entry in the browse-tree API response.
type treeNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`   // repo-relative path (no leading slash)
	IsDir bool   `json:"is_dir"` // true when the node is a folder to descend into
	// Component is non-empty for a terminal artifact node ("groupId:artifactId").
	// The client treats it as a leaf: clicking loads its versions in the center
	// pane rather than expanding. Mutually exclusive with IsDir.
	Component string `json:"component,omitempty"`
	// Severity is the worst severity across the artifact's versions ("" = not
	// vulnerable / not an artifact node). Only set on Component (leaf) nodes.
	Severity string `json:"severity,omitempty"`
}

// mavenComponent builds the "groupId:artifactId" identifier for an artifact
// directory named artifact sitting under the given repo-relative prefix
// (groupId path with "/" separators).
func mavenComponent(prefix, artifact string) string {
	g := strings.ReplaceAll(prefix, "/", ".")
	if g == "" {
		return artifact
	}
	return g + ":" + artifact
}

type componentItem struct {
	Name      string    `json:"name"`
	Versions  []string  `json:"versions"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Severity  string    `json:"severity,omitempty"` // worst severity across versions; "" = not vulnerable
}

type componentsResponse struct {
	Components []componentItem `json:"components"`
	Total      int             `json:"total"`
	Page       int             `json:"page"`
	Limit      int             `json:"limit"`
}

type searchResult struct {
	Repo     string   `json:"repo"`
	Format   string   `json:"format"`
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
	Severity string   `json:"severity,omitempty"` // worst severity across versions; "" = not vulnerable
}

type searchResponse struct {
	Query   string         `json:"query"`
	Results []searchResult `json:"results"`
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// clampedInt parses an integer query parameter, returning def if absent/invalid,
// clamped to [min, max].
func clampedInt(r *http.Request, key string, def, min, max int) int {
	s := r.URL.Query().Get(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
