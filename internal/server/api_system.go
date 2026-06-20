package server

import (
	"net/http"
	"strings"

	"forge/internal/blob"
	"forge/internal/proxy"
	"forge/internal/queue"
)

// handleSystemAPI dispatches GET /api/v1/system/{resource}.
// All system endpoints require admin authentication.
func (s *Server) handleSystemAPI(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	resource := strings.TrimPrefix(r.URL.Path, "/api/v1/system/")
	resource = strings.TrimSuffix(resource, "/")

	switch resource {
	case "health":
		s.systemHealth(w, r)
	case "tasks":
		s.systemTasks(w, r)
	case "request-chart":
		s.systemRequestChart(w, r)
	case "metrics-chart":
		s.systemMetricsChart(w, r)
	case "status-breakdown":
		s.systemStatusBreakdown(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ── GET /api/v1/system/health ────────────────────────────────────────────────

type systemHealthResponse struct {
	UpstreamHealth map[string]string `json:"upstream_health"` // scheme://host → "ok"|"down"
	BlobUsedGB     float64           `json:"blob_used_gb"`
	BlobTotalGB    float64           `json:"blob_total_gb"`
	QueueDepth     int               `json:"queue_depth"`
	RetryInFlight  int32             `json:"retry_in_flight"`
	MetaLatencyMs  float64           `json:"meta_latency_ms"`
}

func (s *Server) systemHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := systemHealthResponse{
		UpstreamHealth: proxy.AllHealth(),
		RetryInFlight:  s.retryGauge.Load(),
	}
	if c, ok := s.Blob.(blob.Capacitor); ok {
		if used, total, err := c.Capacity(); err == nil {
			resp.BlobUsedGB = float64(used) / 1e9
			resp.BlobTotalGB = float64(total) / 1e9
		}
	}
	if dr, ok := s.Queue.(queue.DepthReader); ok {
		resp.QueueDepth = dr.Depth()
	}
	if s.GlobalStats != nil {
		resp.MetaLatencyMs = s.GlobalStats.MetaLatencyMS.Value()
	}
	writeJSON(w, resp)
}

// ── GET /api/v1/system/tasks ─────────────────────────────────────────────────

func (s *Server) systemTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tasks []queue.TaskInfo
	if s.TaskRing != nil {
		tasks = s.TaskRing.Recent(10)
	}
	if tasks == nil {
		tasks = []queue.TaskInfo{} // never return null JSON array
	}
	writeJSON(w, tasks)
}

// ── GET /api/v1/system/request-chart ─────────────────────────────────────────

func (s *Server) systemRequestChart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GlobalStats == nil {
		writeJSON(w, []struct{}{})
		return
	}
	writeJSON(w, s.GlobalStats.RequestChartSnapshot())
}

// ── GET /api/v1/system/metrics-chart ─────────────────────────────────────────

func (s *Server) systemMetricsChart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GlobalStats == nil {
		writeJSON(w, []struct{}{})
		return
	}
	writeJSON(w, s.GlobalStats.MetricsChartSnapshot())
}

// ── GET /api/v1/system/status-breakdown ──────────────────────────────────────

func (s *Server) systemStatusBreakdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.GlobalStats == nil {
		writeJSON(w, []struct{}{})
		return
	}
	writeJSON(w, s.GlobalStats.StatusBreakdown())
}

