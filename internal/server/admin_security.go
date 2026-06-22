package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"forge/internal/repo"
	"forge/internal/vuln"
)

// securityPolicyNames returns a sorted list of named security-policy names for
// dropdowns. Empty when the policy manager is unconfigured.
func (s *Server) securityPolicyNames() []string {
	if s.VulnPolicy == nil {
		return nil
	}
	list, err := s.VulnPolicy.List()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// resolvedSecurity describes the effective policy for a repo and where it came
// from, for display in the Security tab.
type resolvedSecurity struct {
	Policy vuln.Policy `json:"policy"`
	Source string      `json:"source"` // "named:<name>" | "default" | "off"
}

func (s *Server) resolveSecurity(rp repo.Repository) resolvedSecurity {
	if s.VulnPolicy == nil {
		return resolvedSecurity{Policy: vuln.Policy{Mode: vuln.ModeOff}, Source: "off"}
	}
	if rp.SecurityPolicyName != "" {
		if np, ok, _ := s.VulnPolicy.Get(rp.SecurityPolicyName); ok {
			return resolvedSecurity{Policy: np.Policy, Source: "named:" + rp.SecurityPolicyName}
		}
	}
	def, _ := s.VulnPolicy.Default()
	return resolvedSecurity{Policy: def, Source: "default"}
}

// handleSecurityPolicies serves the named-policy CRUD plus the global default.
// Mirrors handleCleanupPolicies. Admin-only.
//
//	GET  /api/v1/security-policies            list named policies
//	POST /api/v1/security-policies            create a named policy
//	GET|PUT /api/v1/security-policies/_default global default policy
//	GET|PUT|DELETE /api/v1/security-policies/{name}
func (s *Server) handleSecurityPolicies(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if s.VulnPolicy == nil {
		http.Error(w, "security policy manager not configured", http.StatusServiceUnavailable)
		return
	}
	seg := strings.TrimPrefix(r.URL.Path, "/api/v1/security-policies")
	seg = strings.TrimPrefix(seg, "/")
	switch {
	case seg == "":
		s.handleSecurityPoliciesList(w, r)
	case seg == "_default":
		s.handleSecurityDefault(w, r)
	default:
		s.handleSecurityPolicyByName(w, r, seg)
	}
}

func (s *Server) handleSecurityPoliciesList(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		policies, err := s.VulnPolicy.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if policies == nil {
			policies = []vuln.NamedPolicy{}
		}
		writeJSON(w, policies)
	case http.MethodPost:
		var p vuln.NamedPolicy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.VulnPolicy.Put(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(p) //nolint:errcheck
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSecurityPolicyByName(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodGet:
		p, ok, err := s.VulnPolicy.Get(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "security policy not found: "+name, http.StatusNotFound)
			return
		}
		writeJSON(w, p)
	case http.MethodPut:
		var p vuln.NamedPolicy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		p.Name = name // URL name wins
		if err := s.VulnPolicy.Put(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, p)
	case http.MethodDelete:
		if err := s.VulnPolicy.Delete(name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSecurityDefault(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		def, err := s.VulnPolicy.Default()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, def)
	case http.MethodPut:
		var p vuln.Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.VulnPolicy.SetDefault(p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRepoSecurityPolicy serves /api/v1/repos/{name}/security-policy.
// GET returns the resolved effective policy + source; PUT assigns a named
// policy (body {"policyName":"..."}; "" = inherit the global default).
func (s *Server) handleRepoSecurityPolicy(w http.ResponseWriter, r *http.Request, name string) {
	if s.VulnPolicy == nil {
		http.Error(w, "security policy manager not configured", http.StatusServiceUnavailable)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "repository not found: "+name, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.resolveSecurity(rp))
	case http.MethodPut:
		var body struct {
			PolicyName string `json:"policyName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.PolicyName != "" {
			if _, ok, _ := s.VulnPolicy.Get(body.PolicyName); !ok {
				http.Error(w, "security policy not found: "+body.PolicyName, http.StatusBadRequest)
				return
			}
		}
		rp.SecurityPolicyName = body.PolicyName
		if err := s.Repos.Update(rp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.resolveSecurity(rp))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

