package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"forge/internal/format"
	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

// vulnGateBlocks enforces the repository's vulnerability download policy on a
// read of a primary artifact. It returns true only when the download was
// blocked — a 403 has been written and the caller must stop. For a Warn it adds
// the X-Forge-Vulnerabilities header, records the decision, and returns false so
// the download proceeds.
//
// The gate fails open on every uncertainty (no policy manager, no findings
// store, format not gateable, path not a primary artifact, storage error): a
// vulnerability gate must never turn an infrastructure hiccup into an outage.
// Fail-closed behaviour for *unscanned* artifacts is a deliberate policy choice,
// expressed by Policy.FailOpen and handled in Decision — not here.
func (s *Server) vulnGateBlocks(w http.ResponseWriter, r *http.Request, rp repo.Repository, h format.Handler, sub string) bool {
	if s.VulnPolicy == nil || s.Vuln == nil {
		return false
	}
	gate, ok := h.(format.VulnGate)
	if !ok {
		return false // format has no credible OSV source; never gated
	}
	pol, err := s.VulnPolicy.Resolve(rp.SecurityPolicyName)
	if err != nil || pol.Mode == vuln.ModeOff || pol.Mode == "" {
		return false
	}
	component, version, ok := gate.VulnGateTarget(sub)
	if !ok {
		return false // metadata / checksum / index — not a primary artifact
	}
	f, found, err := s.Vuln.Get(rp.Name, component, version)
	if err != nil {
		return false // storage error: fail open
	}
	action, sev := pol.Decision(f, found)
	switch action {
	case vuln.ActionBlock:
		s.recordVulnGate(r, rp, component, version, sev, vuln.ActionBlock)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error":     "download blocked by vulnerability policy",
			"component": component,
			"version":   version,
			"severity":  sev.String(),
			"advisory":  firstAdvisoryURL(f),
		})
		return true
	case vuln.ActionWarn:
		w.Header().Set("X-Forge-Vulnerabilities", component+"@"+version+"; severity="+sev.String())
		s.recordVulnGate(r, rp, component, version, sev, vuln.ActionWarn)
		return false
	default:
		return false
	}
}

// recordVulnGate records a gate decision across the three governance surfaces:
// the durable audit log, the policy.violation webhook, and (for blocks) the
// forge_downloads_blocked_total metric. All side effects are best-effort and off
// the critical path of serving (or refusing) the artifact.
func (s *Server) recordVulnGate(r *http.Request, rp repo.Repository, component, version string, sev vuln.Severity, action vuln.Action) {
	actor := actorLabel(r, s.Auth)
	status := http.StatusOK
	if action == vuln.ActionBlock {
		status = http.StatusForbidden
	}
	if s.AuditLog != nil {
		s.AuditLog.Append(obs.AuditEntry{
			Timestamp: time.Now().UTC(),
			Actor:     actor,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    status,
			Detail:    "vuln-policy: " + action.String() + " " + sev.String() + " " + component + "@" + version,
		})
	}
	if action == vuln.ActionBlock && s.Metrics != nil && s.Metrics.DownloadsBlocked != nil {
		s.Metrics.DownloadsBlocked.WithLabelValues(rp.Name).Inc()
	}
	if s.Webhooks != nil {
		ev := webhook.Event{
			Type:      webhook.EventPolicyViolation,
			Repo:      rp.Name,
			Format:    rp.Format,
			Path:      component + "@" + version,
			Actor:     actor,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"action":    action.String(),
				"severity":  sev.String(),
				"component": component,
				"version":   version,
			},
		}
		go s.Webhooks.Dispatch(context.Background(), ev)
	}
}

// firstAdvisoryURL returns a representative advisory link for a blocked download
// (the worst-severity advisory with a URL), so the 403 body can point the user
// at why. Empty when none of the advisories carry a URL.
func firstAdvisoryURL(f vuln.Finding) string {
	best := ""
	bestSev := vuln.SeverityUnknown
	for _, a := range f.Advisories {
		if a.URL != "" && a.Severity >= bestSev {
			best, bestSev = a.URL, a.Severity
		}
	}
	return best
}
