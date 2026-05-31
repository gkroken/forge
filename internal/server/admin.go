package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

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
