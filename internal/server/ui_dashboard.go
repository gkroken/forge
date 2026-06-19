package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ── page types ────────────────────────────────────────────────────────────────

type dashboardPage struct {
	Title           string
	ActiveNav       string
	RepoCount       int
	FormatCount     int
	TotalRequests   int64
	CacheHitPct     float64
	ReposByFormat   []formatStat
	ReqBars         []reqBar
	RecentActivity  []activityRow
	BackgroundTasks []taskRow
	StoredGB        float64
	LatencyP50Ms    int64
	LatencyP95Ms    int64
	ThroughputRPS   float64
}

type formatStat struct {
	Format string
	Count  int
	Pct    int
	Color  string
	SizeGB float64 // from blob walker; 0 until first walk completes
}

type reqBar struct {
	Hit  int
	Miss int
}

type activityRow struct {
	Icon     string // Material Symbols icon name
	DotClass string // kept for templates that still use it
	Text     string
	Who      string
	When     string
}

type taskRow struct {
	Name   string
	Status string
	Color  string
	Pct    int
}

type observabilityPage struct {
	Title           string
	ActiveNav       string
	TotalRequests   int64
	CacheHits       int64
	CacheMisses     int64
	ErrorPct        float64
	LatencyP50Ms    int64
	LatencyP95Ms    int64
	ThroughputRPS   float64
	RateBars        []rateBar
	StatusBreakdown []statusSlice
	AuditLog        []auditRow
}

type rateBar struct {
	H int // height percentage
}

type statusSlice struct {
	Code   string
	Label  string
	Pct    float64
	PctStr string
	Color  string
}

type auditRow struct {
	Time        string
	Actor       string
	Initials    string // up to 2 chars derived from Actor
	Action      string // semantic verb: Published, Uploaded, Deleted, …
	Target      string // short path excerpt
	Method      string
	MethodColor string
	Path        string
	Status      string
	OK          bool
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

func (s *Server) uiDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	repos := s.Repos.All()

	// Count repos per format for the storage/health panel.
	fmtCounts := make(map[string]int)
	for _, rp := range repos {
		fmtCounts[rp.Format]++
	}

	fmtColors := map[string]string{
		"maven": "#c2693f",
		"npm":   "#b5453f",
		"helm":  "#2a6f9e",
		"cran":  "#3a7ca5",
		"oci":   "#5566b5",
	}

	bsizes := s.GetBlobSizes()
	storedGB := float64(bsizes.TotalBytes) / (1 << 30)

	total := len(repos)
	var fmtStats []formatStat
	for _, f := range []string{"maven", "npm", "helm", "cran", "oci"} {
		n := fmtCounts[f]
		if n == 0 {
			continue
		}
		sizeBytes := bsizes.ByFormat[f]
		sizeGB := float64(sizeBytes) / (1 << 30)
		pct := 0
		if bsizes.TotalBytes > 0 {
			pct = int(float64(sizeBytes) / float64(bsizes.TotalBytes) * 100)
		} else if total > 0 {
			pct = n * 100 / total
		}
		color, ok := fmtColors[f]
		if !ok {
			color = "var(--accent)"
		}
		fmtStats = append(fmtStats, formatStat{Format: f, Count: n, Pct: pct, Color: color, SizeGB: sizeGB})
	}
	for f, n := range fmtCounts {
		if _, known := fmtColors[f]; !known {
			sizeBytes := bsizes.ByFormat[f]
			sizeGB := float64(sizeBytes) / (1 << 30)
			pct := 0
			if bsizes.TotalBytes > 0 {
				pct = int(float64(sizeBytes) / float64(bsizes.TotalBytes) * 100)
			} else if total > 0 {
				pct = n * 100 / total
			}
			fmtStats = append(fmtStats, formatStat{Format: f, Count: n, Pct: pct, Color: "var(--text-muted)", SizeGB: sizeGB})
		}
	}

	var totalReqs int64
	var cacheHits, cacheMisses int64
	if s.reg != nil {
		totalReqs = s.gatherCounterTotal("forge_http_requests_total")
		cacheHits = s.gatherCounterTotal("forge_proxy_cache_hits_total")
		cacheMisses = s.gatherCounterTotal("forge_proxy_cache_misses_total")
	}

	var hitPct float64
	if denom := cacheHits + cacheMisses; denom > 0 {
		hitPct = float64(cacheHits) / float64(denom) * 100
	}
	_ = cacheMisses

	var recentActivity []activityRow
	if s.AuditLog != nil {
		methodVerb := map[string]string{
			"POST": "Published", "PUT": "Uploaded",
			"DELETE": "Deleted", "PATCH": "Updated",
		}
		methodIcon := map[string]string{
			"POST": "cloud_upload", "PUT": "upload",
			"DELETE": "delete", "PATCH": "edit",
		}
		for _, e := range s.AuditLog.Recent(5) {
			dot := "dot-ok"
			if e.Status >= 400 {
				dot = "dot-err"
			}
			path := e.Path
			if len(path) > 42 {
				path = path[:39] + "…"
			}
			verb, ok := methodVerb[e.Method]
			if !ok {
				verb = e.Method
			}
			icon := methodIcon[e.Method]
			if icon == "" {
				icon = "info"
			}
			if e.Status >= 400 {
				icon = "warning"
			}
			recentActivity = append(recentActivity, activityRow{
				Icon:     icon,
				DotClass: dot,
				Text:     verb + " " + path,
				Who:      e.Actor,
				When:     e.Timestamp.UTC().Format("15:04"),
			})
		}
	}

	var latP50, latP95 int64
	var rps float64
	if s.Metrics != nil {
		latP50 = s.Metrics.Latency.P50()
		latP95 = s.Metrics.Latency.P95()
		rps = s.Metrics.Throughput.RatePerSec()
	}

	render(w, tmplDashboard, "admin_shell.html", dashboardPage{
		Title:          "Dashboard",
		ActiveNav:      "dashboard",
		RepoCount:      total,
		FormatCount:    len(fmtCounts),
		TotalRequests:  totalReqs,
		CacheHitPct:    hitPct,
		ReposByFormat:  fmtStats,
		ReqBars:        buildRepresentativeBars(24),
		RecentActivity: recentActivity,
		StoredGB:       storedGB,
		LatencyP50Ms:   latP50,
		LatencyP95Ms:   latP95,
		ThroughputRPS:  rps,
	})
}

// ── Observability ─────────────────────────────────────────────────────────────

func (s *Server) uiObservability(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	var totalReqs, cacheHits, cacheMisses, errReqs int64
	if s.reg != nil {
		totalReqs = s.gatherCounterTotal("forge_http_requests_total")
		cacheHits = s.gatherCounterTotal("forge_proxy_cache_hits_total")
		cacheMisses = s.gatherCounterTotal("forge_proxy_cache_misses_total")
		errReqs = s.gatherCounterByLabelPrefix("forge_http_requests_total", "status", "5")
	}

	var errPct float64
	if totalReqs > 0 {
		errPct = float64(errReqs) / float64(totalReqs) * 100
	}

	var breakdown []statusSlice
	if totalReqs > 0 {
		add := func(prefix, code, label, color string) {
			n := s.gatherCounterByLabelPrefix("forge_http_requests_total", "status", prefix)
			if n == 0 {
				return
			}
			pct := float64(n) / float64(totalReqs) * 100
			breakdown = append(breakdown, statusSlice{
				Code: code, Label: label, Pct: pct,
				PctStr: fmt.Sprintf("%.1f%%", pct), Color: color,
			})
		}
		add("2", "2xx", "Success",      "#2e8b6f")
		add("3", "3xx", "Redirect",     "#3a6ea5")
		add("4", "4xx", "Client error", "#c08a2d")
		add("5", "5xx", "Server error", "#c0503f")
	}

	var latP50, latP95 int64
	var rps float64
	if s.Metrics != nil {
		latP50 = s.Metrics.Latency.P50()
		latP95 = s.Metrics.Latency.P95()
		rps = s.Metrics.Throughput.RatePerSec()
	}

	methodColors := map[string]string{
		"POST": "var(--dot-ok)", "PUT": "#c08a2d",
		"DELETE": "#c0503f", "PATCH": "#c08a2d",
	}
	methodVerbs := map[string]string{
		"POST": "Published", "PUT": "Uploaded",
		"DELETE": "Deleted", "PATCH": "Updated",
		"GET": "Downloaded",
	}
	var auditEntries []auditRow
	if s.AuditLog != nil {
		for _, e := range s.AuditLog.Recent(100) {
			color := methodColors[e.Method]
			if color == "" {
				color = "var(--text-muted)"
			}
			action := methodVerbs[e.Method]
			if action == "" {
				action = e.Method
			}
			if e.Status >= 400 {
				action = "Denied"
			}
			target := auditTarget(e.Path)
			auditEntries = append(auditEntries, auditRow{
				Time:        e.Timestamp.UTC().Format("15:04:05"),
				Actor:       e.Actor,
				Initials:    actorInitials(e.Actor),
				Action:      action,
				Target:      target,
				Method:      e.Method,
				MethodColor: color,
				Path:        e.Path,
				Status:      strconv.Itoa(e.Status),
				OK:          e.Status < 400,
			})
		}
	}

	render(w, tmplObservability, "admin_shell.html", observabilityPage{
		Title:           "Observability",
		ActiveNav:       "observability",
		TotalRequests:   totalReqs,
		CacheHits:       cacheHits,
		CacheMisses:     cacheMisses,
		ErrorPct:        errPct,
		LatencyP50Ms:    latP50,
		LatencyP95Ms:    latP95,
		ThroughputRPS:   rps,
		RateBars:        buildRepresentativeBars32(),
		StatusBreakdown: breakdown,
		AuditLog:        auditEntries,
	})
}

// ── Prometheus helpers ────────────────────────────────────────────────────────

// gatherCounterTotal sums all counter samples for a given metric name.
func (s *Server) gatherCounterTotal(name string) int64 {
	if s.reg == nil {
		return 0
	}
	mfs, err := s.reg.Gather()
	if err != nil {
		return 0
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if c := m.GetCounter(); c != nil {
				total += c.GetValue()
			}
		}
	}
	return int64(total)
}

// gatherCounterByLabelPrefix sums counters where labelName's value starts with prefix.
func (s *Server) gatherCounterByLabelPrefix(name, labelName, prefix string) int64 {
	if s.reg == nil {
		return 0
	}
	mfs, err := s.reg.Gather()
	if err != nil {
		return 0
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelName && len(lp.GetValue()) > 0 && lp.GetValue()[:1] == prefix {
					if c := m.GetCounter(); c != nil {
						total += c.GetValue()
					}
				}
			}
		}
	}
	return int64(total)
}

// ── audit helpers ─────────────────────────────────────────────────────────────

// actorInitials derives up to 2 uppercase initials from a token description.
// "ci-publish-token" → "CP", "anonymous" → "AN", "Alice B" → "AB".
func actorInitials(actor string) string {
	if actor == "" {
		return "??"
	}
	// split on spaces and dashes
	words := strings.FieldsFunc(actor, func(r rune) bool { return r == ' ' || r == '-' || r == '_' })
	if len(words) >= 2 {
		return strings.ToUpper(string([]rune(words[0])[:1]) + string([]rune(words[1])[:1]))
	}
	runes := []rune(actor)
	if len(runes) >= 2 {
		return strings.ToUpper(string(runes[:2]))
	}
	return strings.ToUpper(actor)
}

// auditTarget extracts a short human-readable target from a request path.
// "/repository/releases/com/example/app/1.0/app-1.0.jar" → "app-1.0.jar"
func auditTarget(path string) string {
	if path == "" {
		return "—"
	}
	// strip trailing slash
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndexByte(path, '/')
	if idx >= 0 && idx < len(path)-1 {
		leaf := path[idx+1:]
		if len(leaf) > 48 {
			return leaf[:45] + "…"
		}
		return leaf
	}
	if len(path) > 48 {
		return path[:45] + "…"
	}
	return path
}

// ── chart helpers ─────────────────────────────────────────────────────────────

// buildRepresentativeBars returns a 24-bar bell-curve pattern for the 24h chart.
func buildRepresentativeBars(n int) []reqBar {
	pattern := []int{38, 40, 44, 48, 56, 62, 70, 78, 84, 88, 86, 82, 80, 84, 78, 74, 80, 76, 68, 60, 54, 48, 42, 40}
	bars := make([]reqBar, n)
	for i := 0; i < n; i++ {
		h := pattern[i%len(pattern)]
		hit := h * 82 / 100
		miss := h - hit
		bars[i] = reqBar{Hit: hit, Miss: miss}
	}
	return bars
}

// buildRepresentativeBars32 returns a 32-bar pattern for the observability chart.
func buildRepresentativeBars32() []rateBar {
	pattern := []int{40, 44, 42, 48, 52, 50, 58, 64, 60, 68, 72, 70, 78, 82, 80, 86, 90, 84, 88, 92, 85, 80, 76, 82, 78, 70, 64, 58, 52, 48, 44, 42}
	bars := make([]rateBar, 32)
	for i := 0; i < 32; i++ {
		bars[i] = rateBar{H: pattern[i%len(pattern)]}
	}
	return bars
}
