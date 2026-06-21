package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forge/internal/cleanup"
	"forge/internal/obs"
	"forge/internal/proxy"
	"forge/internal/repo"
)

// repoRequest is the JSON body for create/update repo endpoints.
type repoRequest struct {
	Name          string   `json:"name"`
	Format        string   `json:"format"`
	Kind          string   `json:"kind"`
	Upstream      string   `json:"upstream,omitempty"`
	Members       []string `json:"members,omitempty"`
	AnonymousRead bool     `json:"anonymousRead"`
	ProxyTTL      string   `json:"proxyTTL,omitempty"`  // e.g. "24h" (legacy alias for contentMaxAge)
	ProxyAuth     string   `json:"proxyAuth,omitempty"` // e.g. "Bearer tok"
	// BE-D fields — all optional; absent means keep existing / use server default.
	Enabled        *bool    `json:"enabled,omitempty"`
	BlobStore      string   `json:"blobStore,omitempty"`
	ContentMaxAge  string   `json:"contentMaxAge,omitempty"`
	MetadataMaxAge string   `json:"metadataMaxAge,omitempty"`
	NegativeCache  *bool    `json:"negativeCache,omitempty"`
	AutoBlock      *bool    `json:"autoBlock,omitempty"`
	TimeoutSecs    *int     `json:"timeoutSecs,omitempty"`
	Retries        *int     `json:"retries,omitempty"`
	QuotaGB        *float64 `json:"quotaGB,omitempty"`
}

func (req repoRequest) toRepository() (repo.Repository, error) {
	r := repo.Repository{
		Name:          req.Name,
		Format:        req.Format,
		Kind:          repo.Kind(req.Kind),
		Upstream:      req.Upstream,
		Members:       req.Members,
		AnonymousRead: req.AnonymousRead,
		ProxyAuth:     req.ProxyAuth,
		BlobStore:     req.BlobStore,
		NegativeCache: req.NegativeCache,
		AutoBlock:     req.AutoBlock,
		TimeoutSecs:   req.TimeoutSecs,
		Retries:       req.Retries,
		QuotaGB:       req.QuotaGB,
		Enabled:       true, // default: new repos are online
	}
	if req.Enabled != nil {
		r.Enabled = *req.Enabled
	}
	switch {
	case req.ContentMaxAge != "":
		d, err := time.ParseDuration(req.ContentMaxAge)
		if err != nil {
			return repo.Repository{}, fmt.Errorf("invalid contentMaxAge: %w", err)
		}
		r.ContentMaxAge = &d
	case req.ProxyTTL != "":
		d, err := time.ParseDuration(req.ProxyTTL)
		if err != nil {
			return repo.Repository{}, fmt.Errorf("invalid proxyTTL: %w", err)
		}
		r.ProxyTTL = d
		r.ContentMaxAge = &d
	}
	if req.MetadataMaxAge != "" {
		d, err := time.ParseDuration(req.MetadataMaxAge)
		if err != nil {
			return repo.Repository{}, fmt.Errorf("invalid metadataMaxAge: %w", err)
		}
		r.MetadataMaxAge = &d
	}
	return r, nil
}

func validateRepo(r repo.Repository) string {
	if r.Name == "" {
		return "name is required"
	}
	if r.Format == "" {
		return "format is required"
	}
	switch r.Kind {
	case repo.Hosted, repo.Proxy, repo.Group:
	default:
		return "kind must be hosted, proxy, or group"
	}
	if r.Kind == repo.Proxy && r.Upstream == "" {
		return "upstream is required for proxy repositories"
	}
	if r.Kind == repo.Group && len(r.Members) == 0 {
		return "members is required for group repositories"
	}
	return ""
}

// validateGroupPolicy rejects a public group (anonymousRead=true) whose
// members include a private repo (anonymousRead=false). Without this check
// an anonymous client can read private artifacts through the group.
func validateGroupPolicy(group repo.Repository, mgr *repo.Manager) string {
	if group.Kind != repo.Group || !group.AnonymousRead {
		return ""
	}
	for _, memberName := range group.Members {
		member, ok := mgr.Get(memberName)
		if !ok {
			continue // unknown member — let the handler return 404
		}
		if !member.AnonymousRead {
			return fmt.Sprintf(
				"group %q has anonymousRead=true but member %q has anonymousRead=false: "+
					"anonymous clients would read private content through the group",
				group.Name, memberName,
			)
		}
	}
	return ""
}

// validateMemberPolicy rejects disabling anonymousRead on a repo that is
// already included in a public group.
func validateMemberPolicy(updated repo.Repository, mgr *repo.Manager) string {
	if updated.AnonymousRead {
		return ""
	}
	for _, g := range mgr.All() {
		if g.Kind != repo.Group || !g.AnonymousRead {
			continue
		}
		for _, m := range g.Members {
			if m == updated.Name {
				return fmt.Sprintf(
					"cannot set anonymousRead=false on %q: public group %q would expose it to anonymous clients; "+
						"update the group first",
					updated.Name, g.Name,
				)
			}
		}
	}
	return ""
}

// handleAdminRepos dispatches /api/v1/repos, /api/v1/repos/{name}, and
// /api/v1/repos/{name}/components (browse — no admin required).
func (s *Server) handleAdminRepos(w http.ResponseWriter, r *http.Request) {
	// Strip prefix to get the sub-path (may be empty, a name, or name/sub-resource).
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/repos")
	name = strings.TrimPrefix(name, "/")

	// /api/v1/repos/{name}/components — browse endpoint, no admin required.
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "components" {
		s.handleComponents(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/cleanup — trigger retention policy (admin only).
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "cleanup" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleCleanup(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/cache-stats — hourly hit/miss ring buffer (admin only).
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "cache-stats" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleCacheStats(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/invalidate — flush proxy cache for one repo (admin only).
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "invalidate" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleInvalidate(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/health — circuit-breaker state for the repo's upstream.
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "health" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleRepoHealth(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/reindex — queue an index rebuild (stub).
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "reindex" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleReindex(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/access — token grants targeting this repo.
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "access" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleRepoAccess(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/component — delete one component+version (admin only).
	// Format-agnostic: takes ?name= & ?version= so it works for every format,
	// not just the npm tarball path.
	if repoName, rest, found := strings.Cut(name, "/"); found && rest == "component" {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleDeleteComponent(w, r, repoName)
		return
	}

	// /api/v1/repos/{name}/cache/{key...} — expire a single proxy cache entry.
	if repoName, rest, found := strings.Cut(name, "/"); found && strings.HasPrefix(rest, "cache/") {
		if !s.Enforcer.RequireAdmin(w, r) {
			return
		}
		s.handleExpireCache(w, r, repoName, strings.TrimPrefix(rest, "cache/"))
		return
	}

	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}

	if name == "" {
		switch r.Method {
		case http.MethodGet:
			s.listRepos(w)
		case http.MethodPost:
			s.createRepo(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRepo(w, name)
	case http.MethodPut:
		s.updateRepo(w, r, name)
	case http.MethodDelete:
		s.deleteRepo(w, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listRepos(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Repos.All())
}

func (s *Server) getRepo(w http.ResponseWriter, name string) {
	r, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(r)
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	var req repoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	newRepo, err := req.toRepository()
	if err != nil {
		http.Error(w, "invalid proxyTTL: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg := validateRepo(newRepo); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if msg := validateGroupPolicy(newRepo, s.Repos); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := s.Repos.Add(newRepo); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(newRepo)
}

func (s *Server) updateRepo(w http.ResponseWriter, r *http.Request, name string) {
	var req repoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Name in URL takes precedence over body.
	req.Name = name
	updated, err := req.toRepository()
	if err != nil {
		http.Error(w, "invalid proxyTTL: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg := validateRepo(updated); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if msg := validateGroupPolicy(updated, s.Repos); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if msg := validateMemberPolicy(updated, s.Repos); msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if err := s.Repos.Update(updated); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

func (s *Server) deleteRepo(w http.ResponseWriter, name string) {
	if err := s.Repos.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCleanup handles POST /api/v1/repos/{name}/cleanup.
// Add ?dry=true to preview candidates without deleting.
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}

	var p *repo.CleanupPolicy
	if rp.CleanupPolicyName != "" && s.Cleanup != nil {
		np, found, err := s.Cleanup.Get(rp.CleanupPolicyName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if found {
			p = np.ToCleanupPolicy()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	start := time.Now()
	if r.URL.Query().Get("dry") == "true" {
		result, err := cleanup.DryRun(rp.Name, rp.Format, p, s.Blob, s.Meta)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if result.Candidates == nil {
			result.Candidates = []cleanup.Candidate{}
		}
		var freed int64
		for _, c := range result.Candidates {
			freed += c.SizeBytes
		}
		_ = cleanup.RecordRun(s.Meta, rp.Name, cleanup.CleanupRun{
			Timestamp:  start,
			PolicyName: rp.CleanupPolicyName,
			Deleted:    len(result.Candidates),
			FreedBytes: freed,
			DurationMs: time.Since(start).Milliseconds(),
			DryRun:     true,
		})
		json.NewEncoder(w).Encode(result)
		return
	}
	result, err := cleanup.Run(rp.Name, rp.Format, p, s.Blob, s.Meta)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = cleanup.RecordRun(s.Meta, rp.Name, cleanup.CleanupRun{
		Timestamp:  start,
		PolicyName: rp.CleanupPolicyName,
		Deleted:    result.Deleted,
		FreedBytes: result.FreedBytes,
		DurationMs: time.Since(start).Milliseconds(),
	})
	json.NewEncoder(w).Encode(result)
}

// handleDeleteComponent deletes one component+version from a hosted repo.
// DELETE /api/v1/repos/{name}/component?name={component}&version={version}
func (s *Server) handleDeleteComponent(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	component := r.URL.Query().Get("name")
	version := r.URL.Query().Get("version")
	if component == "" || version == "" {
		http.Error(w, "name and version are required", http.StatusBadRequest)
		return
	}
	res, err := cleanup.DeleteVersion(rp.Name, rp.Format, component, version, s.Blob, s.Meta)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// handleCleanupPolicies dispatches /api/v1/cleanup-policies and
// /api/v1/cleanup-policies/{name}.
func (s *Server) handleCleanupPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if s.Cleanup == nil {
		http.Error(w, "cleanup policy manager not configured", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/cleanup-policies")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		s.handleCleanupPoliciesList(w, r)
		return
	}
	s.handleCleanupPolicyByName(w, r, name)
}

func (s *Server) handleCleanupPoliciesList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		policies, err := s.Cleanup.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if policies == nil {
			policies = []cleanup.NamedPolicy{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(policies)
	case http.MethodPost:
		var p cleanup.NamedPolicy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Cleanup.Put(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCleanupPolicyByName(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		p, ok, err := s.Cleanup.Get(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "cleanup policy not found: "+name, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	case http.MethodPut:
		var p cleanup.NamedPolicy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		p.Name = name // URL name takes precedence over body
		if err := s.Cleanup.Put(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	case http.MethodDelete:
		if err := s.Cleanup.Delete(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCacheStats serves GET /api/v1/repos/{name}/cache-stats.
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	if rp.Kind != repo.Proxy {
		http.Error(w, "cache stats only available for proxy repositories", http.StatusBadRequest)
		return
	}
	var snap obs.StatsSnapshot
	if v, loaded := s.repoStats.Load(name); loaded {
		snap = v.(*obs.RepoStats).Snapshot()
	} else {
		snap = obs.StatsSnapshot{Hourly: make([]obs.HourlyBucket, 24)}
		for i := range snap.Hourly {
			snap.Hourly[i].Hour = i
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap) //nolint:errcheck
}

// handleInvalidate serves POST /api/v1/repos/{name}/invalidate.
// Deletes all proxy-cached blobs and their meta entries for the named repo.
func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.Repos.Get(name); !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	cacheNS := name + ":proxy"
	keys, err := s.Meta.List(cacheNS)
	if err != nil {
		http.Error(w, "failed to list cache entries: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var deleted int
	for _, key := range keys {
		if key == proxy.HealthKey {
			continue
		}
		s.Blob.Delete(key)          //nolint:errcheck
		s.Meta.Delete(cacheNS, key) //nolint:errcheck
		deleted++
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"deleted": deleted}) //nolint:errcheck
}

// handleRepoHealth serves GET /api/v1/repos/{name}/health.
// Returns the circuit-breaker state for the repo's upstream URL.
func (s *Server) handleRepoHealth(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	state := "Unknown"
	if rp.Kind == repo.Proxy && rp.Upstream != "" {
		switch proxy.HealthOf(rp.Upstream) {
		case "ok":
			state = "Closed"
		case "down":
			state = "Open"
		}
	}
	writeJSON(w, map[string]string{"state": state})
}

// handleReindex serves POST /api/v1/repos/{name}/reindex.
// Stub: logs the intent and returns queued status.
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.Repos.Get(name); !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"status": "queued"})
}

// repoAccessGrant is one principal→role binding returned by /access.
type repoAccessGrant struct {
	TokenID     string `json:"token_id"`
	Description string `json:"description"`
	Role        string `json:"role"`
	Type        string `json:"type"` // always "token" for now
}

// handleRepoAccess serves GET /api/v1/repos/{name}/access.
func (s *Server) handleRepoAccess(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.Repos.Get(name); !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	grants := []repoAccessGrant{}
	if s.Auth != nil {
		tokens, _ := s.Auth.List()
		for _, t := range tokens {
			for _, g := range t.Grants {
				if g.Repo == name || g.Repo == "*" {
					grants = append(grants, repoAccessGrant{
						TokenID:     t.ID,
						Description: t.Description,
						Role:        g.Role.String(),
						Type:        "token",
					})
					break
				}
			}
		}
	}
	writeJSON(w, grants)
}

// handleExpireCache serves DELETE /api/v1/repos/{name}/cache/{key...}.
// Removes the blob and cache-meta entry for a specific proxy-cached path.
func (s *Server) handleExpireCache(w http.ResponseWriter, r *http.Request, name, cacheKey string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.Repos.Get(name); !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	cacheNS := name + ":proxy"
	blobKey := name + "/" + cacheKey
	s.Meta.Delete(cacheNS, blobKey) //nolint:errcheck
	s.Blob.Delete(blobKey)          //nolint:errcheck
	writeJSON(w, map[string]bool{"ok": true})
}

// handleAuditAPI serves GET /api/v1/audit?repo=&limit=.
func (s *Server) handleAuditAPI(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repoFilter := r.URL.Query().Get("repo")
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	type auditEntry struct {
		Time     string `json:"time"`
		Actor    string `json:"actor"`
		Initials string `json:"initials"`
		Method   string `json:"method"`
		Path     string `json:"path"`
		Status   int    `json:"status"`
		OK       bool   `json:"ok"`
	}

	entries := []auditEntry{}
	if s.AuditLog != nil {
		needle := ""
		if repoFilter != "" {
			needle = "/" + repoFilter + "/"
		}
		for _, e := range s.AuditLog.Recent(200) {
			if needle != "" && !strings.Contains(e.Path, needle) {
				continue
			}
			entries = append(entries, auditEntry{
				Time:     e.Timestamp.UTC().Format("15:04:05"),
				Actor:    e.Actor,
				Initials: actorInitials(e.Actor),
				Method:   e.Method,
				Path:     e.Path,
				Status:   e.Status,
				OK:       e.Status < 400,
			})
			if len(entries) >= limit {
				break
			}
		}
	}
	writeJSON(w, entries)
}

// handleBlobStores serves GET /api/v1/blob-stores.
// Returns a static single-store list until multi-store registry is implemented.
func (s *Server) handleBlobStores(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, []map[string]string{{"name": "default"}})
}
