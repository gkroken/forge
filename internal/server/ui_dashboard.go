package server

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"forge/internal/blob"
	"forge/internal/obs"
	"forge/internal/proxy"
	"forge/internal/queue"
)

var (
	replicaOnce sync.Once
	replicaName string
)

// replicaID returns this process's identity for per-replica UI labels. In
// Kubernetes the hostname is the pod name; falls back to "local" when unknown.
// The dashboard/observability metrics panels are pod-local (GlobalStats etc.),
// so showing which replica served the page keeps numbers from being mistaken
// for fleet totals — Prometheus/Grafana is the cross-replica source of truth.
func replicaID() string {
	replicaOnce.Do(func() {
		if h, err := os.Hostname(); err == nil && h != "" {
			replicaName = h
		} else {
			replicaName = "local"
		}
	})
	return replicaName
}

// ── page types ────────────────────────────────────────────────────────────────

type dashboardPage struct {
	Title           string
	ActiveNav       string
	Mode            string // EVAL MODE / AUTH ENABLED — instrument-panel header
	Replica         string // serving pod/host id; metrics panels are per-replica
	Uptime          string // HH:MM:SS since process start
	StatusLabel     string // OPERATIONAL / DEGRADED
	StatusDot       string // dot-ok / dot-warn
	StatusOK        bool
	RepoCount       int
	FormatCount     int
	TotalRequests   int64
	CacheHitPct     float64
	ReposByFormat   []formatStat
	ReqBars         []reqBar
	RecentActivity  []activityRow
	BackgroundTasks []taskRow
	HealthRows      []healthRow
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

type healthRow struct {
	DotClass  string // "dot-ok" | "dot-warn" | "dot-err" | "dot-neutral"
	Label     string
	Stat      string
	StatClass string // CSS class for the stat value
}

type observabilityPage struct {
	Title           string
	ActiveNav       string
	Replica         string // serving pod/host id; metrics panels are per-replica
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

	// ── Request chart + totals from GlobalStats (preferred) ──────────────────
	var reqBars []reqBar
	var totalReqs int64
	var hitPct float64

	if s.GlobalStats != nil {
		snap := s.GlobalStats.RequestChartSnapshot()
		reqBars, totalReqs, hitPct = buildRequestBars(snap)
	} else if s.reg != nil {
		// Fall back to Prometheus counters when GlobalStats is not available.
		totalReqs = s.gatherCounterTotal("forge_http_requests_total")
		cacheHits := s.gatherCounterTotal("forge_proxy_cache_hits_total")
		cacheMisses := s.gatherCounterTotal("forge_proxy_cache_misses_total")
		if denom := cacheHits + cacheMisses; denom > 0 {
			hitPct = float64(cacheHits) / float64(denom) * 100
		}
		reqBars = buildRepresentativeBars(24)
	} else {
		reqBars = buildRepresentativeBars(24)
	}

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

	// ── instrument-panel header: mode, uptime, derived health status ──
	mode := "EVAL MODE"
	if s.Auth != nil {
		mode = "AUTH ENABLED"
	}
	uptime := "—"
	if !s.started.IsZero() {
		uptime = humanUptime(time.Since(s.started))
	}
	healthRows := buildHealthRows(s, latP50)
	statusOK := true
	for _, hr := range healthRows {
		switch hr.DotClass {
		case "", "dot-ok", "dot-neutral":
		default:
			statusOK = false
		}
	}
	statusLabel, statusDot := "OPERATIONAL", "dot-ok"
	if !statusOK {
		statusLabel, statusDot = "DEGRADED", "dot-warn"
	}

	render(w, tmplDashboard, "admin_shell.html", dashboardPage{
		Title:           "Dashboard",
		ActiveNav:       "dashboard",
		Mode:            mode,
		Replica:         replicaID(),
		Uptime:          uptime,
		StatusLabel:     statusLabel,
		StatusDot:       statusDot,
		StatusOK:        statusOK,
		RepoCount:       total,
		FormatCount:     len(fmtCounts),
		TotalRequests:   totalReqs,
		CacheHitPct:     hitPct,
		ReposByFormat:   fmtStats,
		ReqBars:         reqBars,
		RecentActivity:  recentActivity,
		BackgroundTasks: buildTaskRows(s),
		HealthRows:      healthRows,
		StoredGB:        storedGB,
		LatencyP50Ms:    latP50,
		LatencyP95Ms:    latP95,
		ThroughputRPS:   rps,
	})
}

// ── Observability ─────────────────────────────────────────────────────────────

func (s *Server) uiObservability(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	// ── Status breakdown: prefer GlobalStats (reset-on-restart), fall back to Prometheus ──
	var breakdown []statusSlice
	var totalReqs, errReqs int64
	if s.GlobalStats != nil {
		colors := map[string]string{"2xx": "#2e8b6f", "304": "#3a6ea5", "4xx": "#c08a2d", "5xx": "#c0503f"}
		for _, e := range s.GlobalStats.StatusBreakdown() {
			if e.Count == 0 {
				continue
			}
			totalReqs += int64(e.Count)
			if e.Code == "5xx" {
				errReqs += int64(e.Count)
			}
			breakdown = append(breakdown, statusSlice{
				Code: e.Code, Label: e.Label, Pct: e.Pct,
				PctStr: fmt.Sprintf("%.1f%%", e.Pct), Color: colors[e.Code],
			})
		}
	} else if s.reg != nil {
		totalReqs = s.gatherCounterTotal("forge_http_requests_total")
		cacheHits := s.gatherCounterTotal("forge_proxy_cache_hits_total")
		_ = cacheHits
		errReqs = s.gatherCounterByLabelPrefix("forge_http_requests_total", "status", "5")
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
			add("2", "2xx", "Success", "#2e8b6f")
			add("3", "3xx", "Redirect", "#3a6ea5")
			add("4", "4xx", "Client error", "#c08a2d")
			add("5", "5xx", "Server error", "#c0503f")
		}
	}

	var errPct float64
	if totalReqs > 0 {
		errPct = float64(errReqs) / float64(totalReqs) * 100
	}

	var latP50, latP95 int64
	var rps float64
	if s.Metrics != nil {
		latP50 = s.Metrics.Latency.P50()
		latP95 = s.Metrics.Latency.P95()
		rps = s.Metrics.Throughput.RatePerSec()
	}

	var auditEntries []auditRow
	if s.AuditLog != nil {
		for _, e := range s.AuditLog.Recent(100) {
			auditEntries = append(auditEntries, buildAuditRow(e, "15:04:05"))
		}
	}

	var rateBars []rateBar
	if s.GlobalStats != nil {
		rateBars = buildMetricsBars(s.GlobalStats.MetricsChartSnapshot())
	} else {
		rateBars = buildRepresentativeBars32()
	}

	render(w, tmplObservability, "admin_shell.html", observabilityPage{
		Title:           "Observability",
		ActiveNav:       "observability",
		Replica:         replicaID(),
		TotalRequests:   totalReqs,
		ErrorPct:        errPct,
		LatencyP50Ms:    latP50,
		LatencyP95Ms:    latP95,
		ThroughputRPS:   rps,
		RateBars:        rateBars,
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

var auditMethodColors = map[string]string{
	"POST": "var(--dot-ok)", "PUT": "#c08a2d",
	"DELETE": "#c0503f", "PATCH": "#c08a2d",
}
var auditMethodVerbs = map[string]string{
	"POST": "Published", "PUT": "Uploaded",
	"DELETE": "Deleted", "PATCH": "Updated",
	"GET": "Downloaded",
}

// buildAuditRow maps an audit entry to its display row, formatting the timestamp
// with the given layout (recent views use a time-only layout; the history page
// uses a full date-time so entries spanning days stay unambiguous).
func buildAuditRow(e obs.AuditEntry, timeLayout string) auditRow {
	color := auditMethodColors[e.Method]
	if color == "" {
		color = "var(--text-muted)"
	}
	action := auditMethodVerbs[e.Method]
	if action == "" {
		action = e.Method
	}
	if e.Status >= 400 {
		action = "Denied"
	}
	return auditRow{
		Time:        e.Timestamp.UTC().Format(timeLayout),
		Actor:       e.Actor,
		Initials:    actorInitials(e.Actor),
		Action:      action,
		Target:      auditTarget(e.Path),
		Method:      e.Method,
		MethodColor: color,
		Path:        e.Path,
		Status:      strconv.Itoa(e.Status),
		OK:          e.Status < 400,
	}
}

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

// buildRepresentativeBars returns a 24-bar placeholder when GlobalStats is unavailable.
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

// buildRepresentativeBars32 returns a 32-bar placeholder when GlobalStats is unavailable.
func buildRepresentativeBars32() []rateBar {
	pattern := []int{40, 44, 42, 48, 52, 50, 58, 64, 60, 68, 72, 70, 78, 82, 80, 86, 90, 84, 88, 92, 85, 80, 76, 82, 78, 70, 64, 58, 52, 48, 44, 42}
	bars := make([]rateBar, 32)
	for i := 0; i < 32; i++ {
		bars[i] = rateBar{H: pattern[i%len(pattern)]}
	}
	return bars
}

// buildRequestBars converts GlobalStats hourly snapshot into chart bars and totals.
func buildRequestBars(snap []obs.HourlyRequestBucket) ([]reqBar, int64, float64) {
	var maxTotal, totalReqs, totalHits, totalMisses uint64
	for _, b := range snap {
		t := b.Requests
		if t > maxTotal {
			maxTotal = t
		}
		totalReqs += b.Requests
		totalHits += b.CacheHits
		totalMisses += b.CacheMisses
	}
	bars := make([]reqBar, len(snap))
	for i, b := range snap {
		if maxTotal == 0 {
			bars[i] = reqBar{}
			continue
		}
		total := b.Requests
		hitH := 0
		if total > 0 && b.CacheHits > 0 {
			hitH = int(b.CacheHits * 100 / maxTotal)
		}
		missH := int(total*100/maxTotal) - hitH
		if missH < 0 {
			missH = 0
		}
		bars[i] = reqBar{Hit: hitH, Miss: missH}
	}
	var hitPct float64
	if denom := totalHits + totalMisses; denom > 0 {
		hitPct = float64(totalHits) / float64(denom) * 100
	}
	return bars, int64(totalReqs), hitPct
}

// buildMetricsBars converts GlobalStats 15-min snapshot into rate bars (height 0–100).
func buildMetricsBars(snap []obs.MetricsBucket) []rateBar {
	var maxRate float64
	for _, b := range snap {
		if b.ReqRate > maxRate {
			maxRate = b.ReqRate
		}
	}
	bars := make([]rateBar, len(snap))
	for i, b := range snap {
		h := 0
		if maxRate > 0 {
			h = int(b.ReqRate / maxRate * 100)
		}
		if h < 0 {
			h = 0
		}
		bars[i] = rateBar{H: h}
	}
	return bars
}

// buildTaskRows converts TaskRing entries into display rows.
func buildTaskRows(s *Server) []taskRow {
	if s.TaskRing == nil {
		return nil
	}
	tasks := s.TaskRing.Recent(10)
	rows := make([]taskRow, 0, len(tasks))
	for _, t := range tasks {
		var color string
		var pct int
		switch t.Status {
		case "running":
			color = "var(--accent)"
			pct = 50 // unknown progress; show half-filled
		case "done":
			color = "var(--dot-ok)"
			pct = 100
		case "failed":
			color = "var(--dot-err)"
			pct = 100
		default:
			color = "var(--text-muted)"
		}
		rows = append(rows, taskRow{
			Name:   t.Name,
			Status: t.Status,
			Color:  color,
			Pct:    pct,
		})
	}
	return rows
}

// buildHealthRows assembles the service health panel from live system state.
// humanUptime renders a process uptime as the instrument-panel readout
// "HH:MM:SS", prefixed with whole days once it crosses 24h.
func humanUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	days := total / 86400
	h := (total % 86400) / 3600
	m := (total % 3600) / 60
	sec := total % 60
	if days > 0 {
		return fmt.Sprintf("%dd %02d:%02d:%02d", days, h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
}

func buildHealthRows(s *Server, latP50Ms int64) []healthRow {
	var rows []healthRow

	// REST API latency
	apiStat := "ok"
	apiClass := "text-ok"
	if latP50Ms > 0 {
		apiStat = fmt.Sprintf("ok · %dms p50", latP50Ms)
	}
	rows = append(rows, healthRow{DotClass: "dot-ok", Label: "REST API", Stat: apiStat, StatClass: apiClass})

	// Blob store capacity
	if c, ok := s.Blob.(blob.Capacitor); ok {
		if used, total, err := c.Capacity(); err == nil && total > 0 {
			pct := float64(used) / float64(total) * 100
			dot, cls := "dot-ok", "text-ok"
			if pct > 90 {
				dot, cls = "dot-err", "text-err"
			} else if pct > 75 {
				dot, cls = "dot-warn", "text-warn"
			}
			rows = append(rows, healthRow{
				DotClass:  dot,
				Label:     "Blob store",
				Stat:      fmt.Sprintf("%.0f%% used", pct),
				StatClass: cls,
			})
		}
	}

	// Async queue depth
	if dr, ok := s.Queue.(queue.DepthReader); ok {
		depth := dr.Depth()
		dot := "dot-ok"
		if depth > 50 {
			dot = "dot-warn"
		}
		stat := "idle"
		if depth > 0 {
			stat = fmt.Sprintf("%d queued", depth)
		}
		rows = append(rows, healthRow{DotClass: dot, Label: "Async indexer", Stat: stat, StatClass: "text-ok"})
	}

	// Proxy retry gauge
	if retrying := s.retryGauge.Load(); retrying > 0 {
		rows = append(rows, healthRow{
			DotClass:  "dot-warn",
			Label:     "Proxy workers",
			Stat:      fmt.Sprintf("%d retrying", retrying),
			StatClass: "text-warn",
		})
	} else {
		rows = append(rows, healthRow{DotClass: "dot-ok", Label: "Proxy workers", Stat: "idle", StatClass: "text-ok"})
	}

	// Proxy upstream health (one row per unique upstream host that has been contacted)
	for host, state := range proxy.AllHealth() {
		dot, cls := "dot-ok", "text-ok"
		if state == "down" {
			dot, cls = "dot-err", "text-err"
		}
		label := host
		if len(label) > 32 {
			label = label[:29] + "…"
		}
		rows = append(rows, healthRow{DotClass: dot, Label: label, Stat: state, StatClass: cls})
	}

	return rows
}
