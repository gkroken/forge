package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/format"
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
	blobPrefix := rp.Name + "/"
	if prefix != "" {
		blobPrefix += prefix + "/"
	}

	keys, err := s.Blob.List(blobPrefix)
	if err != nil {
		jsonError(w, "list failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Collect immediate children (one level deep) deduplicating directories.
	seen := map[string]bool{}
	var nodes []treeNode
	for _, k := range keys {
		rel := strings.TrimPrefix(k, blobPrefix)
		if rel == "" {
			continue
		}
		seg, rest, isDir := strings.Cut(rel, "/")
		if seg == "" {
			continue
		}
		if seen[seg] {
			continue
		}
		seen[seg] = true
		nodePath := prefix
		if nodePath != "" {
			nodePath += "/" + seg
		} else {
			nodePath = seg
		}
		_ = rest
		nodes = append(nodes, treeNode{Name: seg, Path: nodePath, IsDir: isDir})
	}
	if nodes == nil {
		nodes = []treeNode{}
	}
	writeJSON(w, nodes)
}

// handleComponents serves GET /api/v1/repos/{name}/components
//
//	?q=      substring filter on component name (case-insensitive)
//	?page=   1-based page number (default 1)
//	?limit=  page size (default 50, max 200)
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
	limit := clampedInt(r, "limit", 50, 1, 200)
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

	items := make([]componentItem, len(entries))
	for i, e := range entries {
		items[i] = componentItem{Name: e.Name, Versions: e.Versions, UpdatedAt: e.UpdatedAt}
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
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Name), ql) {
				results = append(results, searchResult{
					Repo:     rp.Name,
					Format:   rp.Format,
					Name:     e.Name,
					Versions: e.Versions,
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
					resp.Versions = append(resp.Versions, browseVersionRow{Version: v})
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

	resp := browseDetailResponse{
		Name:    pkg,
		Version: ver,
		Format:  rp.Format,
		Repo:    repoName,
	}
	for _, v := range detail.Versions {
		if v.Version == ver {
			resp.PublishedAt = v.PublishedAt
			resp.DownloadURL = v.DownloadURL
			break
		}
	}
	if resp.DownloadURL == "" {
		resp.DownloadURL = publicBase(r) + "/repository/" + repoName + "/" + pkg
	}
	writeJSON(w, resp)
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
}

type browseDetailResponse struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Format      string    `json:"format"`
	Repo        string    `json:"repo"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	DownloadURL string    `json:"download_url"`
}

// treeNode is one entry in the browse-tree API response.
type treeNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`   // repo-relative path (no leading slash)
	IsDir bool   `json:"is_dir"` // true when the node has children
}

type componentItem struct {
	Name      string    `json:"name"`
	Versions  []string  `json:"versions"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
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
