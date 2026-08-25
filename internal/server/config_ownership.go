package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"forge/internal/config"
	"forge/internal/obs"
)

// Config-as-code enforcement.
//
// When forge runs with -config, the file is the source of truth for the objects
// it manages. Editing one of those through the admin API or UI would put git and
// reality out of step with no signal, so those writes are refused with 409.
//
// Ownership is per object, not a global mode: objects created through the API
// stay fully editable. This is the Grafana provisioning model (a provisioned
// dashboard is read-only and badged; a UI-made one is not), with Kubernetes'
// per-object field ownership rather than an all-or-nothing switch.

// ManagedBy values reported by the admin API.
const (
	managedByConfig = "config"
	managedByAPI    = "api"
)

// WithConfigMode marks the server as running under config-as-code. source is
// the config file path (echoed in refusals so an operator knows where to make
// the change). allowOverride is the break-glass: writes are permitted but
// logged and audited, and the next apply reverts them.
func (s *Server) WithConfigMode(source string, allowOverride bool) *Server {
	s.configSource = source
	s.configOverride = allowOverride
	return s
}

// configOwns reports whether the named object is managed by the config file.
// Always false when forge is not in -config mode.
func (s *Server) configOwns(kind, name string) bool {
	if s.configSource == "" || s.Meta == nil {
		return false
	}
	return config.LoadOwnership(s.Meta).Owns(kind, name)
}

// managedBy labels an object for API responses.
func (s *Server) managedBy(kind, name string) string {
	if s.configOwns(kind, name) {
		return managedByConfig
	}
	return managedByAPI
}

// refuseConfigOwned gates one mutating request. It reports true when the caller
// must stop (the response has been written).
//
// With -allow-config-override the write proceeds, but it is logged and audited.
// No extra bookkeeping is needed to "mark it drifted": the object is still in
// the managed set, so the next Plan sees managed+differs and reports it as a
// pending Update — which is exactly what drift means here.
func (s *Server) refuseConfigOwned(w http.ResponseWriter, r *http.Request, kind, name string) bool {
	if !s.configOwns(kind, name) {
		return false
	}
	if s.configOverride {
		slog.Warn("config: override write to config-managed object",
			"kind", kind, "name", name, "method", r.Method, "actor", actorLabel(r, s.Auth))
		s.auditConfigEvent(r, http.StatusOK, fmt.Sprintf(
			"config override: %s %s %q edited outside %s; next apply will revert it",
			r.Method, kind, name, s.configSource))
		return false
	}
	s.auditConfigEvent(r, http.StatusConflict, fmt.Sprintf(
		"refused %s to config-managed %s %q", r.Method, kind, name))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":     "object is managed by config",
		"kind":      kind,
		"name":      name,
		"source":    s.configSource,
		"remedy":    "edit " + s.configSource + " and redeploy, or start forge with -allow-config-override",
		"managedBy": managedByConfig,
	})
	return true
}

// auditConfigEvent records a config-enforcement decision. Best-effort.
func (s *Server) auditConfigEvent(r *http.Request, status int, detail string) {
	if s.AuditLog == nil {
		return
	}
	s.AuditLog.Append(obs.AuditEntry{
		Timestamp: time.Now().UTC(),
		Actor:     actorLabel(r, s.Auth),
		Method:    r.Method,
		Path:      r.URL.Path,
		Status:    status,
		Detail:    detail,
	})
}

// withManagedBy marshals v and injects a "managedBy" key.
//
// It goes through a map rather than embedding v in a wrapper struct because
// repo.Repository defines MarshalJSON: an embedded value promotes that method,
// and the wrapper's own fields are silently dropped from the output.
func withManagedBy(v any, managedBy string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return json.RawMessage(b) // not an object (shouldn't happen); pass through
	}
	m["managedBy"] = json.RawMessage(strconv.Quote(managedBy))
	out, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(b)
	}
	return out
}
