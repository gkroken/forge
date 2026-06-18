package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"forge/internal/cleanup"
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
	ProxyTTL      string   `json:"proxyTTL,omitempty"`  // e.g. "24h"
	ProxyAuth     string   `json:"proxyAuth,omitempty"` // e.g. "Bearer tok"
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
	}
	if req.ProxyTTL != "" {
		d, err := time.ParseDuration(req.ProxyTTL)
		if err != nil {
			return repo.Repository{}, err
		}
		r.ProxyTTL = d
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
	if r.URL.Query().Get("dry") == "true" {
		result, err := cleanup.DryRun(rp.Name, rp.Format, p, s.Blob, s.Meta)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if result.Candidates == nil {
			result.Candidates = []cleanup.Candidate{}
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	result, err := cleanup.Run(rp.Name, rp.Format, p, s.Blob, s.Meta)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(result)
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
