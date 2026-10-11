package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"forge/internal/config"
	"forge/internal/cranplatform"
)

// handleCRANPlatforms serves the CRAN Linux platform registry:
//
//	GET    /api/v1/cran/platforms               list registrations
//	GET    /api/v1/cran/platforms/{key}         one registration
//	PUT    /api/v1/cran/platforms/{key}         register or replace
//	DELETE /api/v1/cran/platforms/{key}         unregister
//
// A key contains a slash ("noble-x86_64/4.6"), so everything after the prefix
// is the key. Writes are global admin only: a registration decides what every
// binary repository accepts for that environment. Reads are open to any
// authenticated caller: the registration decides which binaries a client is
// served, so a client must be able to check it names the image it expects
// before installing from or uploading to that platform's tree.
func (s *Server) handleCRANPlatforms(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if !s.Enforcer.RequireAuthenticated(w, r) {
			return
		}
	} else if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	reg := cranplatform.NewStore(s.Meta)
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/cran/platforms"), "/")

	if key == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		list, err := reg.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]json.RawMessage, 0, len(list))
		for _, p := range list {
			out = append(out, withManagedBy(p, s.managedBy(config.KindCRANPlatform, p.Key)))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, ok, err := reg.Get(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "CRAN platform not registered: "+key, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(withManagedBy(p, s.managedBy(config.KindCRANPlatform, key)))
	case http.MethodPut:
		var p cranplatform.Platform
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if p.Key != "" && p.Key != key {
			http.Error(w, "body key "+p.Key+" does not match the URL key "+key, http.StatusBadRequest)
			return
		}
		p.Key = key
		if s.refuseConfigOwned(w, r, config.KindCRANPlatform, key) {
			return
		}
		if err := reg.Put(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	case http.MethodDelete:
		if s.refuseConfigOwned(w, r, config.KindCRANPlatform, key) {
			return
		}
		if _, ok, _ := reg.Get(key); !ok {
			http.Error(w, "CRAN platform not registered: "+key, http.StatusNotFound)
			return
		}
		if err := reg.Delete(key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
