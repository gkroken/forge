package obs

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus instruments for forge.
// One instance is created at startup and threaded through the server.
type Metrics struct {
	// HTTP layer
	HTTPRequests *prometheus.CounterVec   // {method, route, status}
	HTTPDuration *prometheus.HistogramVec // {method, route}

	// Proxy cache
	CacheHits   *prometheus.CounterVec // {repo}
	CacheMisses *prometheus.CounterVec // {repo}

	// Index regen queue
	QueueJobsTotal *prometheus.CounterVec // {type, result}

	// Artifact downloads per repo
	Downloads *prometheus.CounterVec // {repo}

	// Downloads refused by the vulnerability policy gate, per repo.
	DownloadsBlocked *prometheus.CounterVec // {repo}

	// Webhook deliveries by outcome (one per attempt): success | failed | dropped
	WebhookDeliveries *prometheus.CounterVec // {result}

	// Current count of vulnerable components by repo and worst-severity bucket.
	// A gauge (not a counter): it reflects present state, re-set after each scan.
	VulnerableComponents *prometheus.GaugeVec // {repo, severity}

	// In-process latency + throughput (not Prometheus instruments)
	Latency    *LatencyTracker
	Throughput *ThroughputTracker
}

// NewMetrics registers all instruments with reg and returns the populated struct.
// Panics if any registration fails (programmer error — duplicate name).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_http_requests_total",
			Help: "Total HTTP requests by method, route pattern, and status code.",
		}, []string{"method", "route", "status"}),

		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "forge_http_request_duration_seconds",
			Help:    "HTTP request latency by method and route pattern.",
			Buckets: []float64{.005, .025, .1, .5, 1, 2.5, 5},
		}, []string{"method", "route"}),

		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_proxy_cache_hits_total",
			Help: "Proxy requests served from the local cache.",
		}, []string{"repo"}),

		CacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_proxy_cache_misses_total",
			Help: "Proxy requests that required an upstream fetch.",
		}, []string{"repo"}),

		QueueJobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_queue_jobs_total",
			Help: "Index-regeneration jobs processed by the background worker.",
		}, []string{"type", "result"}),

		Downloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_artifact_downloads_total",
			Help: "Successful artifact GET responses (HTTP 200) per repository.",
		}, []string{"repo"}),

		DownloadsBlocked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_downloads_blocked_total",
			Help: "Artifact downloads refused by the vulnerability policy gate, per repository.",
		}, []string{"repo"}),

		WebhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "forge_webhook_deliveries_total",
			Help: "Webhook delivery attempts by outcome (success, failed, dropped).",
		}, []string{"result"}),

		VulnerableComponents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "forge_vulnerable_components",
			Help: "Vulnerable components by repository and worst-severity bucket (current state).",
		}, []string{"repo", "severity"}),
	}

	m.Latency = NewLatencyTracker(1000)
	m.Throughput = &ThroughputTracker{}

	reg.MustRegister(
		// Go runtime + process metrics
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		// Application metrics
		m.HTTPRequests,
		m.HTTPDuration,
		m.CacheHits,
		m.CacheMisses,
		m.QueueJobsTotal,
		m.Downloads,
		m.DownloadsBlocked,
		m.WebhookDeliveries,
		m.VulnerableComponents,
	)
	return m
}

// SetVulnerableComponents re-publishes the vulnerable-component gauge for repo
// from a worst-severity histogram (severity label → count). It clears repo's
// prior series first so a severity that dropped to zero stops being exported.
// Takes a plain map so obs stays decoupled from the vuln package.
func (m *Metrics) SetVulnerableComponents(repo string, bySeverity map[string]int) {
	if m == nil || m.VulnerableComponents == nil {
		return
	}
	m.VulnerableComponents.DeletePartialMatch(prometheus.Labels{"repo": repo})
	for sev, n := range bySeverity {
		m.VulnerableComponents.WithLabelValues(repo, sev).Set(float64(n))
	}
}

// Handler returns an HTTP handler that exposes the registry in the Prometheus
// text exposition format, suitable for mounting at /metrics.
func Handler(reg prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
}
