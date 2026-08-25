package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"forge/internal/config"
)

// Config-as-code drift.
//
// Apply runs at boot, so between a git commit and the next rollout the file and
// the live state can disagree. Nothing surfaced that: Argo/Flux track the
// ConfigMap, not forge's own objects, so an OutOfSync repository looked green.
//
// This exposes the same diff Plan already computes — as an endpoint for humans
// and tooling, and as a gauge for Prometheus.

// configPlanner re-reads the config file and diffs it against live state.
// nil when forge is not running under -config.
type configPlanner func() (config.Result, error)

// WithConfigDrift supplies the planner used by /api/v1/config/drift and the
// drift gauge. The planner re-reads the file on every call, so an updated
// ConfigMap is picked up without a restart.
func (s *Server) WithConfigDrift(p configPlanner) *Server {
	s.configPlan = p
	return s
}

// driftResponse is the API shape. Counts mirror config.KindResult so a client
// can tell "3 repos would be updated" from "3 repos would be deleted".
type driftResponse struct {
	Drift     bool              `json:"drift"`   // true when anything would change
	Objects   int               `json:"objects"` // total pending write operations
	Source    string            `json:"source"`  // the config file path
	CheckedAt time.Time         `json:"checkedAt"`
	Kinds     map[string]kindOp `json:"kinds"`
	Conflicts []config.Conflict `json:"conflicts"` // adoptions blocked without adopt
}

type kindOp struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Deleted int `json:"deleted"`
	Adopted int `json:"adopted"`
	Noop    int `json:"noop"`
}

func toKindOp(k config.KindResult) kindOp {
	return kindOp{Created: k.Created, Updated: k.Updated, Deleted: k.Deleted,
		Adopted: k.Adopted, Noop: k.Noop}
}

// handleConfigDrift serves GET /api/v1/config/drift. Read-only; admin-scoped.
// 404 when forge was not started with -config — there is no file to diff.
func (s *Server) handleConfigDrift(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if s.configPlan == nil {
		http.Error(w, "forge is not running in config-as-code mode", http.StatusNotFound)
		return
	}
	res, err := s.computeDrift()
	if err != nil {
		// A file that no longer loads or validates is itself actionable — report
		// it rather than pretending there is no drift.
		http.Error(w, "config drift check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// computeDrift runs the planner and updates the drift gauge as a side effect,
// so a scrape right after an API call agrees with what the caller saw.
func (s *Server) computeDrift() (driftResponse, error) {
	res, err := s.configPlan()
	if err != nil {
		return driftResponse{}, err
	}
	out := driftResponse{
		Source:    s.configSource,
		CheckedAt: time.Now().UTC(),
		Kinds:     map[string]kindOp{},
		Conflicts: res.Conflicts,
	}
	if out.Conflicts == nil {
		out.Conflicts = []config.Conflict{}
	}
	for kind, k := range map[string]config.KindResult{
		"repositories":     res.Repositories,
		"roles":            res.Roles,
		"cleanupPolicies":  res.CleanupPolicies,
		"securityPolicies": res.SecurityPolicies,
		"webhooks":         res.Webhooks,
	} {
		out.Kinds[kind] = toKindOp(k)
		out.Objects += k.Changes()
	}
	out.Drift = out.Objects > 0
	s.publishDriftMetrics(out)
	return out, nil
}

// publishDriftMetrics mirrors the drift result into forge_config_drift_objects.
func (s *Server) publishDriftMetrics(d driftResponse) {
	if s.Metrics == nil || s.Metrics.ConfigDriftObjects == nil {
		return
	}
	g := s.Metrics.ConfigDriftObjects
	for kind, k := range d.Kinds {
		g.WithLabelValues(kind, "create").Set(float64(k.Created))
		g.WithLabelValues(kind, "update").Set(float64(k.Updated))
		g.WithLabelValues(kind, "delete").Set(float64(k.Deleted))
		g.WithLabelValues(kind, "adopt").Set(float64(k.Adopted))
	}
	g.WithLabelValues("all", "conflict").Set(float64(len(d.Conflicts)))
}

// StartDriftWatcher refreshes the drift gauge on an interval so Prometheus sees
// a current value without anyone calling the endpoint. It stops when ctx-less
// done is closed. No-op when forge is not in config mode.
func (s *Server) StartDriftWatcher(every time.Duration, done <-chan struct{}) {
	if s.configPlan == nil {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			// Refresh immediately, then on each tick.
			if d, err := s.computeDrift(); err != nil {
				slog.Warn("config: drift check failed", "err", err)
			} else if d.Drift {
				slog.Info("config: drift detected", "objects", d.Objects, "source", d.Source)
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
}
