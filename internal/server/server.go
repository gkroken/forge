// Package server wires HTTP requests to repositories and their format handlers.
//
// URL scheme (same shape Nexus uses):
//
//	/repository/{repo-name}/{format-specific-path...}
//
// The server resolves {repo-name} -> Repository -> Handler(format), then hands
// off. It deliberately knows nothing about Maven/npm/etc.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/indexer"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/oidc"
	"forge/internal/queue"
	"forge/internal/repo"
)

// ociError writes an OCI Distribution Spec error response.
func ociError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"code": code, "message": message}},
	})
}

// defaultMaxUpload is the default per-request body limit (5 GiB).
// Large enough for any realistic artifact; prevents runaway clients from
// exhausting disk/S3 capacity. Override with Server.MaxUpload.
const defaultMaxUpload = 5 << 30

type Server struct {
	Repos     *repo.Manager
	Handlers  *format.Registry
	Blob      blob.Store
	Meta      meta.Store
	Auth      auth.Store           // nil = auth not enabled (eval mode)
	Enforcer  *auth.Enforcer       // always non-nil; uses AllowAll when Auth is nil
	OIDC      oidcProvider         // nil = OIDC not configured; *oidc.Provider satisfies this
	Queue     queue.Queue          // nil = no async index regen (eval / tests)
	Metrics   *obs.Metrics         // nil = no instrumentation (tests)
	Cleanup   *cleanup.PolicyManager // nil = cleanup-policies API returns 503
	AuditLog  *obs.AuditLog          // nil = no in-memory audit log
	MaxUpload int64                // per-request body limit; 0 = use defaultMaxUpload
	reg       prometheus.Gatherer
	client    *http.Client
	oidcKey   []byte // HMAC key for signing OIDC state cookies; set by WithOIDC
}

func New(m *repo.Manager, reg *format.Registry, b blob.Store, mt meta.Store, a auth.Store) *Server {
	return &Server{
		Repos: m, Handlers: reg, Blob: b, Meta: mt, Auth: a,
		Enforcer:  auth.NewEnforcer(a, m),
		client:    &http.Client{Timeout: 30 * time.Second},
		MaxUpload: defaultMaxUpload,
	}
}

// WithMetrics attaches a Prometheus registry + instruments to the server.
// Call before Routes() so the /metrics endpoint and middleware are active.
func (s *Server) WithMetrics(metrics *obs.Metrics, gatherer prometheus.Gatherer) *Server {
	s.Metrics = metrics
	s.reg = gatherer
	return s
}

// WithOIDC attaches an OIDC provider and generates the HMAC signing key used
// for state cookies.  Call before Routes().
func (s *Server) WithOIDC(p *oidc.Provider) *Server {
	s.OIDC = p
	s.oidcKey = make([]byte, 32)
	if _, err := rand.Read(s.oidcKey); err != nil {
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return s
}

// WithQueue attaches a queue to the server and starts the indexer worker in
// a background goroutine that runs until ctx is cancelled.
func (s *Server) WithQueue(ctx context.Context, q queue.Queue) *Server {
	s.Queue = q
	go indexer.New(s.Meta).WithMetrics(s.Metrics).Work(ctx, q) //nolint:errcheck
	return s
}

func (s *Server) WithCleanup(pm *cleanup.PolicyManager) *Server {
	s.Cleanup = pm
	return s
}

func (s *Server) WithAuditLog(al *obs.AuditLog) *Server {
	s.AuditLog = al
	return s
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleProbe)
	mux.HandleFunc("/readyz", handleProbe)
	if s.reg != nil {
		mux.Handle("/metrics", obs.Handler(s.reg))
	}
	mux.HandleFunc("/api/v1/tokens", s.handleTokens)
	mux.HandleFunc("/api/v1/tokens/", s.handleTokens)
	mux.HandleFunc("/api/v1/cleanup-policies", s.handleCleanupPolicies)
	mux.HandleFunc("/api/v1/cleanup-policies/", s.handleCleanupPolicies)
	mux.HandleFunc("/api/v1/repos", s.handleAdminRepos)
	mux.HandleFunc("/api/v1/repos/", s.handleAdminRepos)
	mux.HandleFunc("/api/v1/search", s.handleSearch)
	if s.OIDC != nil && s.Auth != nil {
		mux.HandleFunc("/auth/oidc/login", s.handleOIDCLogin)
		mux.HandleFunc("/auth/oidc/callback", s.handleOIDCCallback)
	}
	mux.Handle("/ui/static/", s.serveUIStatic())
	mux.HandleFunc("/ui/", s.handleUI)
	// Auth middleware wraps every /repository/ route.
	mux.Handle("/repository/", s.Enforcer.Middleware(http.HandlerFunc(s.handleRepo)))
	// OCI Distribution Spec: /v2/ is the API root.
	// The base check (/v2/ or /v2) is unauthenticated (needed for auth discovery).
	// All other /v2/ routes are protected via MiddlewareOCI.
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.Header().Set("OCI-Distribution-Spec-Version", "1.0.0")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{}\n")
			return
		}
		s.Enforcer.MiddlewareOCI(http.HandlerFunc(s.handleOCI)).ServeHTTP(w, r)
	})
	mux.HandleFunc("/", s.handleIndex)
	return s.middleware(mux)
}

func handleProbe(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "ok\n")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type row struct {
		Name, Format, Kind, URL string
	}
	var rows []row
	for _, rp := range s.Repos.All() {
		rows = append(rows, row{rp.Name, rp.Format, string(rp.Kind),
			fmt.Sprintf("/repository/%s/", rp.Name)})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service":      "forge",
		"repositories": rows,
	})
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/repository/")
	name, sub, _ := strings.Cut(rest, "/")
	if name == "" {
		http.Error(w, "no repository named", http.StatusBadRequest)
		return
	}
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.Error(w, "no such repository: "+name, http.StatusNotFound)
		return
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		http.Error(w, "no handler for format: "+rp.Format, http.StatusNotImplemented)
		return
	}
	h.Serve(w, r, &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client, Sub: sub,
		Repos: s.Repos, Queue: s.Queue, Metrics: s.Metrics,
	})
}

func (s *Server) handleOCI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")

	// /v2/_catalog — list OCI repositories
	if rest == "_catalog" {
		var names []string
		for _, rp := range s.Repos.All() {
			if rp.Format == "oci" {
				names = append(names, rp.Name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"repositories": names})
		return
	}

	repoName, sub, _ := strings.Cut(rest, "/")
	rp, ok := s.Repos.Get(repoName)
	if !ok || rp.Format != "oci" {
		ociError(w, "NAME_UNKNOWN", "repository not found", http.StatusNotFound)
		return
	}
	h, ok := s.Handlers.For("oci")
	if !ok {
		ociError(w, "UNSUPPORTED", "OCI handler not registered", http.StatusNotImplemented)
		return
	}
	h.Serve(w, r, &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
		Sub: sub, Repos: s.Repos, Queue: s.Queue, Metrics: s.Metrics,
	})
}

// statusRecorder captures the HTTP status code written by a handler so the
// middleware can log and record it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) written() int {
	if sr.status == 0 {
		return http.StatusOK
	}
	return sr.status
}

// auditEvent returns true for requests that should be recorded in the audit log:
// authentication failures (401/403) and successful artifact writes/deletes.
func auditEvent(method, path string, status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	if status >= 200 && status < 300 &&
		(method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete) {
		return strings.HasPrefix(path, "/repository/") || strings.HasPrefix(path, "/v2/")
	}
	return false
}

// actorLabel extracts the authenticated actor name from a request for audit
// logging. It re-validates the token (Bearer header or forge_token cookie) so
// the caller doesn't need auth in the request context. Falls back to "anonymous".
func actorLabel(r *http.Request, a auth.Store) string {
	if a == nil {
		return "anonymous"
	}
	var secret string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		secret = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(auth.UISessionCookie); err == nil {
		secret = c.Value
	}
	if secret == "" {
		return "anonymous"
	}
	tok, err := a.Verify(secret)
	if err != nil || tok == nil {
		return "anonymous"
	}
	return tok.Description
}

// routeLabel returns a low-cardinality route label for Prometheus.
// Repo/artifact sub-paths are collapsed to avoid label explosion.
func routeLabel(path string) string {
	switch {
	case path == "/healthz":
		return "/healthz"
	case path == "/readyz":
		return "/readyz"
	case path == "/metrics":
		return "/metrics"
	case strings.HasPrefix(path, "/repository/"):
		return "/repository/{repo}"
	case strings.HasPrefix(path, "/v2/"):
		return "/v2/{repo}"
	case strings.HasPrefix(path, "/api/v1/tokens"):
		return "/api/v1/tokens"
	default:
		return "other"
	}
}

// middleware is the outermost handler: security headers + structured access log
// + Prometheus metrics. Probe endpoints (/healthz, /readyz) are passed through
// without logging to keep log volume low during liveness checks.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		// CSP: allow htmx from unpkg CDN; inline styles needed for templates.
		// frame-ancestors 'none' supersedes X-Frame-Options in modern browsers.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' https://unpkg.com; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'")

		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		// Enforce upload size limit on methods that carry a request body.
		// Content-Length check is a fast path for clients that declare size
		// upfront; MaxBytesReader covers chunked / unknown-length bodies.
		if isWriteMethod(r.Method) {
			limit := s.MaxUpload
			if limit <= 0 {
				limit = defaultMaxUpload
			}
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.written()
		dur := time.Since(start)
		route := routeLabel(r.URL.Path)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", dur.Milliseconds(),
			"remote", r.RemoteAddr,
		)

		if auditEvent(r.Method, r.URL.Path, status) {
			slog.Info("audit",
				"audit", true,
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"remote", r.RemoteAddr,
			)
			if s.AuditLog != nil {
				s.AuditLog.Append(obs.AuditEntry{
					Timestamp: start,
					Actor:     actorLabel(r, s.Auth),
					Method:    r.Method,
					Path:      r.URL.Path,
					Status:    status,
				})
			}
		}

		if s.Metrics != nil {
			s.Metrics.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
			s.Metrics.HTTPDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
		}

	})
}

// isWriteMethod reports whether method carries a request body that counts
// against the upload size limit.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return true
	}
	return false
}
