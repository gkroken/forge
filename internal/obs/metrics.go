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
	)
	return m
}

// Handler returns an HTTP handler that exposes the registry in the Prometheus
// text exposition format, suitable for mounting at /metrics.
func Handler(reg prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
}
