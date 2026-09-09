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
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/indexer"
	"forge/internal/ldap"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/oidc"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/trivy"
	"forge/internal/vuln"
	"forge/internal/webhook"
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

// BlobSizes is a cached snapshot of blob storage usage.
type BlobSizes struct {
	TotalBytes  int64
	ByFormat    map[string]int64
	ByRepo      map[string]int64 // bytes per repo name
	CountByRepo map[string]int   // artifact (blob) count per repo name
	ComputedAt  time.Time
}

type Server struct {
	Repos       *repo.Manager
	Handlers    *format.Registry
	Blob        blob.Store
	Meta        meta.Store
	Auth        auth.Store             // nil = auth not enabled (eval mode)
	Enforcer    *auth.Enforcer         // always non-nil; uses AllowAll when Auth is nil
	OIDC        oidcProvider           // nil = OIDC not configured; *oidc.Provider satisfies this
	GroupMapper *auth.GroupRoleMapper  // nil = no OIDC group→role mapping; SSO logins use fallback grants
	LDAP        ldapAuthenticator      // nil = LDAP not configured; *ldap.Client satisfies this
	ldapMapper  *auth.GroupRoleMapper  // nil = no LDAP group→role mapping; LDAP logins use fallback grants
	Queue       queue.Queue            // nil = no async index regen (eval / tests)
	Metrics     *obs.Metrics           // nil = no instrumentation (tests)
	Cleanup     *cleanup.PolicyManager // nil = cleanup-policies API returns 503
	Scheduler   *cleanup.Scheduler     // nil = no scheduled runs (eval / tests)
	AuditLog    obs.AuditSink          // nil = no audit log (eval: ring buffer; prod: Postgres)
	// configSource is the -config file path when forge runs under
	// config-as-code, "" otherwise. Non-empty enables per-object write
	// refusal for config-managed objects (see config_ownership.go).
	configSource string
	// configOverride is the -allow-config-override break-glass: config-managed
	// objects stay writable, but every such write is logged and audited.
	configOverride bool
	// configPlan re-reads the config file and diffs it against live state.
	// nil outside config-as-code mode (see config_drift.go).
	configPlan     configPlanner
	Users          auth.UserStore      // nil = user management not configured
	Roles          auth.RoleStore      // nil = custom roles not configured
	Webhooks       *webhook.Engine     // nil = webhooks not configured (no event emission)
	Vuln           *vuln.Store         // nil = vulnerability scanning not configured
	OSV            *vuln.Client        // nil = no OSV producer (scans disabled)
	Trivy          *trivy.Scanner      // nil = OCI image scanning not configured
	VulnPolicy     *vuln.PolicyManager // nil = no download-policy gate
	MaxUpload      int64               // per-request body limit; 0 = use defaultMaxUpload
	TrashRetention time.Duration       // soft-delete trash purged after this age; <=0 = keep forever
	reg            prometheus.Gatherer
	client         *http.Client
	oidcKey        []byte // HMAC key for signing OIDC state cookies; set by WithOIDC

	started time.Time // process start; powers the dashboard uptime readout

	blobMu      sync.RWMutex
	blobSizes   BlobSizes
	walkTrigger chan struct{} // non-blocking send kicks off an immediate re-walk
	quotaDelta  sync.Map      // map[string]*atomic.Int64; bytes written per hosted repo since the last walk

	repoStats  sync.Map // map[string]*obs.RepoStats; lazy-init per proxy repo
	retryGauge atomic.Int32

	GlobalStats *obs.GlobalStats
	TaskRing    *queue.TaskRing
}

func New(m *repo.Manager, reg *format.Registry, b blob.Store, mt meta.Store, a auth.Store) *Server {
	return &Server{
		Repos: m, Handlers: reg, Blob: b, Meta: mt, Auth: a,
		Enforcer:  auth.NewEnforcer(a, m),
		client:    &http.Client{Timeout: 30 * time.Second},
		MaxUpload: defaultMaxUpload,
		started:   time.Now(),
	}
}

// WithMetrics attaches a Prometheus registry + instruments to the server.
// Call before Routes() so the /metrics endpoint and middleware are active.
func (s *Server) WithMetrics(metrics *obs.Metrics, gatherer prometheus.Gatherer) *Server {
	s.Metrics = metrics
	s.reg = gatherer
	return s
}

// WithOIDC attaches an OIDC provider and its group→role mapper, and generates
// the HMAC signing key used for state cookies.  mapper may be nil (SSO logins
// then fall back to the provider's default grants).  Call before Routes().
func (s *Server) WithOIDC(p *oidc.Provider, mapper *auth.GroupRoleMapper) *Server {
	s.OIDC = p
	s.GroupMapper = mapper
	s.oidcKey = make([]byte, 32)
	if _, err := rand.Read(s.oidcKey); err != nil {
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return s
}

// WithLDAP attaches an LDAP authenticator and its group→role mapper. The existing
// username/password login form becomes the LDAP entry point (search-then-bind);
// on success a normal forge session token is minted via establishSSOSession. mapper
// may be nil (LDAP logins then fall back to the client's default grants). Call
// before Routes().
func (s *Server) WithLDAP(c *ldap.Client, mapper *auth.GroupRoleMapper) *Server {
	s.LDAP = c
	s.ldapMapper = mapper
	return s
}

// WithTrashRetention sets how long soft-deleted artifacts are kept in trash
// before the scheduler's purge sweep hard-deletes them. A non-positive duration
// keeps trash indefinitely (manual purge only). Call before Routes().
func (s *Server) WithTrashRetention(d time.Duration) *Server {
	s.TrashRetention = d
	return s
}

// WithGlobalStats attaches the server-wide metrics collector.
// Call before Routes() so the middleware and format contexts can record stats.
func (s *Server) WithGlobalStats(gs *obs.GlobalStats) *Server {
	s.GlobalStats = gs
	return s
}

// WithQueue attaches a queue to the server and starts the single async worker in
// a background goroutine that runs until ctx is cancelled. The worker drains the
// shared queue, dispatching index-regen jobs built-in and any registered job
// families (e.g. webhook delivery) via their handlers. Call WithWebhooks before
// WithQueue so the webhook handler is registered before the Work loop starts.
func (s *Server) WithQueue(ctx context.Context, q queue.Queue) *Server {
	s.Queue = q
	s.TaskRing = queue.NewTaskRing(10)
	w := indexer.New(s.Meta).WithMetrics(s.Metrics).WithTaskRing(s.TaskRing)
	if s.Webhooks != nil {
		w.Register(webhook.JobType, s.Webhooks.Handle)
	}
	if s.Vuln != nil && s.OSV != nil {
		w.Register(vulnScanJobType, s.handleVulnScanJob)
	}
	if s.Trivy != nil && s.Vuln != nil {
		w.Register(trivyScanJobType, s.handleTrivyScanJob)
		w.Register(trivyRepoScanJobType, s.handleTrivyRepoScanJob)
		w.Register(helmRepoScanJobType, s.handleHelmRepoScanJob)
	}
	// Integrity verify and Nexus migration need only the stores, so they are
	// always registered.
	w.Register(integrityJobType, s.handleIntegrityJob)
	w.Register(migrationJobType, s.handleMigrationJob)
	go w.Work(ctx, q) //nolint:errcheck
	return s
}

// WithVuln attaches the vulnerability findings store and OSV producer. Call
// before WithQueue (which registers the scan handler) and before Routes() (so
// the publish hook enqueues scans). nil store or client leaves scanning disabled.
func (s *Server) WithVuln(store *vuln.Store, client *vuln.Client) *Server {
	s.Vuln = store
	s.OSV = client
	// Repopulate the vulnerable-component gauge from persisted rollups so it
	// reflects the last scan immediately after a restart, rather than staying
	// empty until the next scan fires.
	if store != nil && s.Repos != nil {
		for _, rp := range s.Repos.All() {
			if r, ok, err := store.GetRollup(rp.Name); err == nil && ok {
				s.Metrics.SetVulnerableComponents(rp.Name, r.BySeverity)
			}
		}
	}
	return s
}

// WithVulnPolicy attaches the security-policy manager that drives the download
// gate. Independent of WithVuln's producer wiring: the gate only needs the
// findings store (Vuln) plus the resolved policy, so a deployment can enforce
// against previously-scanned findings even with the OSV client offline.
func (s *Server) WithVulnPolicy(pm *vuln.PolicyManager) *Server {
	s.VulnPolicy = pm
	return s
}

// WithWebhooks attaches the webhook engine. Call before WithQueue (which
// registers its delivery handler) and before Routes() (so the publish hook emits
// events). nil leaves webhooks disabled.
func (s *Server) WithWebhooks(e *webhook.Engine) *Server {
	s.Webhooks = e
	return s
}

func (s *Server) WithCleanup(pm *cleanup.PolicyManager) *Server {
	s.Cleanup = pm
	return s
}

func (s *Server) WithScheduler(sc *cleanup.Scheduler) *Server {
	s.Scheduler = sc
	return s
}

func (s *Server) WithAuditLog(al obs.AuditSink) *Server {
	s.AuditLog = al
	return s
}

func (s *Server) WithUsers(us auth.UserStore) *Server {
	s.Users = us
	return s
}

func (s *Server) WithRoles(rs auth.RoleStore) *Server {
	s.Roles = rs
	return s
}

// WithBlobWalker starts a background goroutine that periodically computes
// blob storage sizes. Call before Routes().
func (s *Server) WithBlobWalker(ctx context.Context) *Server {
	s.walkTrigger = make(chan struct{}, 1)
	go func() {
		s.walkBlobSizes()
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.walkBlobSizes()
			case <-s.walkTrigger:
				s.walkBlobSizes()
			}
		}
	}()
	return s
}

func (s *Server) walkBlobSizes() {
	byFmt := map[string]int64{}
	byRepo := map[string]int64{}
	countByRepo := map[string]int{}
	total := int64(0)
	// Capture the in-flight quota deltas before the walk reads the store; the
	// freshly-listed sizes already include those bytes, so we subtract the
	// captured amount afterward. Writes that land DURING the walk keep their
	// delta (they may not be reflected in this snapshot yet) — bounded, no loss.
	captured := map[string]int64{}
	s.quotaDelta.Range(func(k, v any) bool {
		captured[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	for _, rp := range s.Repos.All() {
		keys, err := s.Blob.List(rp.Name + "/")
		if err != nil {
			continue
		}
		for _, k := range keys {
			info, ok, err := s.Blob.Stat(k)
			if err != nil || !ok {
				continue
			}
			total += info.Size
			byFmt[rp.Format] += info.Size
			byRepo[rp.Name] += info.Size
			countByRepo[rp.Name]++
		}
	}
	// Group repos own no blobs of their own; surface the union of their members'
	// usage so the lists/dashboard don't show groups as empty. Computed after the
	// per-repo walk and deliberately not added to total (avoids double-counting).
	for _, rp := range s.Repos.All() {
		if rp.Kind != repo.Group {
			continue
		}
		for _, m := range rp.Members {
			byRepo[rp.Name] += byRepo[m]
			countByRepo[rp.Name] += countByRepo[m]
		}
	}
	s.blobMu.Lock()
	s.blobSizes = BlobSizes{
		TotalBytes:  total,
		ByFormat:    byFmt,
		ByRepo:      byRepo,
		CountByRepo: countByRepo,
		ComputedAt:  time.Now(),
	}
	s.blobMu.Unlock()

	// Reconcile the in-flight quota deltas now that their bytes are in byRepo, and
	// re-publish the per-repo quota-usage gauge from the fresh snapshot.
	for name, n := range captured {
		if v, ok := s.quotaDelta.Load(name); ok {
			v.(*atomic.Int64).Add(-n)
		}
	}
	if s.Metrics != nil && s.Metrics.QuotaUsedRatio != nil {
		for _, rp := range s.Repos.All() {
			if rp.Kind != repo.Hosted || rp.QuotaGB == nil || *rp.QuotaGB <= 0 {
				continue
			}
			quotaBytes := *rp.QuotaGB * float64(bytesPerGB)
			s.Metrics.QuotaUsedRatio.WithLabelValues(rp.Name).Set(float64(byRepo[rp.Name]) / quotaBytes)
		}
	}
}

// triggerWalk kicks off an immediate, non-blocking blob re-walk so storage- and
// quota-dependent surfaces reflect a change (e.g. a trash/restore/purge that
// moved bytes in or out of a repo's key space) without waiting for the 5-minute
// tick. No-op when the walker isn't running (eval / tests).
func (s *Server) triggerWalk() {
	if s.walkTrigger == nil {
		return
	}
	select {
	case s.walkTrigger <- struct{}{}:
	default: // a walk is already queued
	}
}

// GetBlobSizes returns the most recent cached blob size snapshot.
func (s *Server) GetBlobSizes() BlobSizes {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	return s.blobSizes
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
	mux.HandleFunc("/api/v1/users", s.handleUsers)
	mux.HandleFunc("/api/v1/users/", s.handleUsers)
	mux.HandleFunc("/api/v1/roles", s.handleRoles)
	mux.HandleFunc("/api/v1/roles/", s.handleRoles)
	mux.HandleFunc("/api/v1/cleanup-policies", s.handleCleanupPolicies)
	mux.HandleFunc("/api/v1/cleanup-policies/", s.handleCleanupPolicies)
	mux.HandleFunc("/api/v1/security-policies", s.handleSecurityPolicies)
	mux.HandleFunc("/api/v1/security-policies/", s.handleSecurityPolicies)
	mux.HandleFunc("/api/v1/repos", s.handleAdminRepos)
	mux.HandleFunc("/api/v1/repos/", s.handleAdminRepos)
	mux.HandleFunc("/api/v1/search", s.handleSearch)
	mux.HandleFunc("/api/v1/integrity", s.handleIntegrityRollup)
	mux.HandleFunc("/api/v1/migration", s.handleMigration)
	mux.HandleFunc("/api/v1/migration/", s.handleMigration)
	mux.HandleFunc("/api/v1/audit", s.handleAuditAPI)
	mux.HandleFunc("/api/v1/blob-stores", s.handleBlobStores)
	mux.HandleFunc("/api/v1/webhooks", s.handleWebhooks)
	mux.HandleFunc("/api/v1/webhooks/", s.handleWebhooks)
	mux.HandleFunc("/api/v1/system/", s.handleSystemAPI)
	mux.HandleFunc("/api/v1/config/drift", s.handleConfigDrift)
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

// repoBlob returns the blob store a repository must be written through.
//
// An immutable repository gets the write-once wrapper. This is a function
// rather than a line inside handleRepo because handleRepo is not the only
// route to the bytes: the browser upload form and the Nexus migration both
// build a format.Context and dispatch to a handler in process, and both handed
// over the raw store — so an "immutable" repository could be overwritten
// through either. A guard that lives in one route is a guard on that route,
// not on the repository.
func (s *Server) repoBlob(rp repo.Repository) blob.Store {
	if rp.IsImmutable() {
		return blob.Immutable(s.Blob)
	}
	return s.Blob
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
	if !rp.Enabled {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"repository offline"}`, http.StatusServiceUnavailable)
		return
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		http.Error(w, "no handler for format: "+rp.Format, http.StatusNotImplemented)
		return
	}
	// Storage-quota gate: refuse writes to a hosted repo at or over its quota.
	if isWriteMethod(r.Method) {
		if s.quotaBlocks(w, r, rp) {
			return
		}
	}
	// Vulnerability download policy gate: only reads of primary artifacts. On a
	// Block this writes a 403 and returns; on Warn it adds a header and proceeds.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if s.vulnGateBlocks(w, r, rp, h, sub) {
			return
		}
	}
	// Dependency-confusion guard (group/proxy repos): refuse claimed names no
	// hosted copy can serve, and keep proxy members out of group fan-outs for
	// protected components. Nil when inactive.
	guard := s.newDepGuard(rp, h, sub)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if guard.blocks(w, r) {
			return
		}
	}
	var repoStats *obs.RepoStats
	if rp.Kind == repo.Proxy {
		v, _ := s.repoStats.LoadOrStore(rp.Name, &obs.RepoStats{})
		repoStats = v.(*obs.RepoStats)
	}
	c := &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client, Sub: sub,
		Repos: s.Repos, Queue: s.Queue, Metrics: s.Metrics,
		RepoStats: repoStats, RepoStatsFn: s.lookupRepoStats,
		GlobalStats: s.GlobalStats, RetryGauge: &s.retryGauge,
		OnCacheFill: s.onProxyCacheFill,
	}
	if guard != nil && rp.Kind == repo.Group {
		c.MemberFilter = guard.memberFilter
		c.NameClaimed = guard.claimedName
	}
	// Write-once enforcement: an immutable hosted repo rejects overwrites of an
	// existing artifact at the blob layer (ErrImmutable → 409 in the handler).
	// OCI is never routed here, so its content-addressed shared-layer re-pushes
	// stay unaffected.
	if rp.IsImmutable() {
		// Refuse deletes up front so the client gets a clear 409 rather than
		// whatever each format makes of ErrImmutable from the store. The
		// wrapper still refuses them underneath, which is what keeps the
		// guarantee true for any future path that reaches the bytes.
		if r.Method == http.MethodDelete {
			http.Error(w, "repository is immutable; artifacts are write-once and cannot be deleted",
				http.StatusConflict)
			return
		}
		c.Blob = s.repoBlob(rp)
	}
	h.Serve(w, r, c)
}

// onProxyCacheFill emits an artifact.cached webhook when a proxy repository
// fills its cache from upstream. The proxy passes the stored blob key
// ("{repo}/{sub}"); the repo is its first segment, so this works for both
// direct proxy repos and proxy members of a group. No-op without webhooks.
func (s *Server) onProxyCacheFill(blobKey string) {
	if s.Webhooks == nil {
		return
	}
	repoName, path, _ := strings.Cut(blobKey, "/")
	if repoName == "" {
		return
	}
	ev := webhook.Event{
		Type:      webhook.EventArtifactCached,
		Repo:      repoName,
		Path:      path,
		Actor:     "proxy",
		Timestamp: time.Now().UTC(),
	}
	if rp, ok := s.Repos.Get(repoName); ok {
		ev.Format = rp.Format
	}
	go s.Webhooks.Dispatch(context.Background(), ev)
}

func (s *Server) lookupRepoStats(name string) *obs.RepoStats {
	if v, ok := s.repoStats.Load(name); ok {
		return v.(*obs.RepoStats)
	}
	return nil
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
	if !rp.Enabled {
		ociError(w, "DENIED", "repository offline", http.StatusServiceUnavailable)
		return
	}
	h, ok := s.Handlers.For("oci")
	if !ok {
		ociError(w, "UNSUPPORTED", "OCI handler not registered", http.StatusNotImplemented)
		return
	}
	// Storage-quota gate: refuse writes to a hosted OCI repo at or over its quota.
	// OCI usage is tracked purely off the periodic walk (the delta accounting is
	// scoped to /repository/ writes, whose bodies map cleanly to stored bytes).
	if isWriteMethod(r.Method) {
		if s.quotaBlocks(w, r, rp) {
			return
		}
	}
	// Vulnerability download policy gate: tag-addressed manifest GETs only.
	// Mirrors the same gate in handleRepo; digest pulls and non-manifest paths
	// fail open (VulnGateTarget returns ok=false for those).
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if s.vulnGateBlocks(w, r, rp, h, sub) {
			return
		}
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

		if s.GlobalStats != nil {
			s.GlobalStats.RecordRequest(status, dur.Milliseconds())
		}

		if s.Metrics != nil {
			s.Metrics.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
			s.Metrics.HTTPDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
			s.Metrics.Latency.Observe(dur)
			s.Metrics.Throughput.Inc()

			// Download counter: GET 200 on a repository artifact path.
			if r.Method == http.MethodGet && status == http.StatusOK &&
				strings.HasPrefix(r.URL.Path, "/repository/") {
				rest := strings.TrimPrefix(r.URL.Path, "/repository/")
				if repoName, _, ok := strings.Cut(rest, "/"); ok && repoName != "" {
					s.Metrics.Downloads.WithLabelValues(repoName).Inc()
				}
			}
		}

		// Record per-artifact download time for last-downloaded retention. Async
		// + throttled so it adds no request latency. Only when cleanup is
		// configured (the sole consumer). The blob key is the path after the
		// /repository/ prefix ("{repo}/{sub}").
		if s.Cleanup != nil && r.Method == http.MethodGet && status == http.StatusOK &&
			strings.HasPrefix(r.URL.Path, "/repository/") {
			blobKey := strings.TrimPrefix(r.URL.Path, "/repository/")
			if blobKey != "" && !strings.HasSuffix(blobKey, "/") {
				go cleanup.RecordDownload(s.Meta, blobKey)
			}
		}

		// After a successful write to a repository: re-walk blob sizes, and if the
		// repo opted into on-publish cleanup, fire a debounced run. Scoped to
		// /repository/ (the four formats cleanup.Run implements); OCI /v2/ has no
		// cleanup support yet. Notify returns immediately — cleanup never runs in
		// the request path.
		if isWriteMethod(r.Method) && status < 300 &&
			strings.HasPrefix(r.URL.Path, "/repository/") {
			if s.walkTrigger != nil {
				select {
				case s.walkTrigger <- struct{}{}:
				default: // walk already queued; skip
				}
			}
			rest := strings.TrimPrefix(r.URL.Path, "/repository/")
			repoName, subPath, _ := strings.Cut(rest, "/")
			// In-flight quota accounting: credit the bytes written to a hosted repo
			// so the soft quota gate reflects this upload before the next blob walk
			// reconciles it. Skipped for unknown-length bodies (ContentLength < 0).
			if r.ContentLength > 0 && repoName != "" {
				if rp, ok := s.Repos.Get(repoName); ok && rp.Kind == repo.Hosted {
					s.addQuotaDelta(repoName, r.ContentLength)
				}
			}
			if s.Scheduler != nil && repoName != "" {
				s.Scheduler.Notify(repoName)
			}
			// Enqueue an OSV scan of the repo. Off the request path and
			// failure-isolated — a missing OSV egress never breaks a publish.
			s.enqueueVulnScan(repoName)
			// Helm chart upload (POST .../api/charts) → Trivy config scan. The
			// chart name/version live in the .tgz body, not the path, so this
			// enqueues a whole-repo scan. Self-gated on helm format + Trivy.
			s.enqueueHelmScan(repoName)
			// Emit an artifact.published webhook event. Off the request path: the
			// engine only enqueues durable delivery jobs here. Background context
			// so the enqueue outlives the request.
			if s.Webhooks != nil && repoName != "" {
				ev := webhook.Event{
					Type:      webhook.EventArtifactPublished,
					Repo:      repoName,
					Path:      subPath,
					Actor:     actorLabel(r, s.Auth),
					Timestamp: start,
				}
				if rp, ok := s.Repos.Get(repoName); ok {
					ev.Format = rp.Format
				}
				go s.Webhooks.Dispatch(context.Background(), ev)
			}
		}

		// OCI manifest PUT under /v2/ makes an image tag- (or digest-) addressable:
		// that is the publish moment. Blob and upload PUTs are NOT publishes, and
		// cleanup/walk don't cover OCI, so this emits the webhook and enqueues a scan.
		if r.Method == http.MethodPut && status >= 200 && status < 300 &&
			strings.HasPrefix(r.URL.Path, "/v2/") {
			repoName, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
			if image, ref, ok := ociManifestRef(sub); ok && repoName != "" {
				if s.Webhooks != nil {
					go s.Webhooks.Dispatch(context.Background(), webhook.Event{
						Type:      webhook.EventArtifactPublished,
						Repo:      repoName,
						Format:    "oci",
						Path:      image + ":" + ref,
						Actor:     actorLabel(r, s.Auth),
						Timestamp: start,
					})
				}
				// Enqueue a Trivy scan for tag-based manifest pushes. Skip digest refs
				// (sha256:...) — trivy image needs a tag, not a content-addressed ref.
				if !strings.HasPrefix(ref, "sha256:") {
					s.enqueueTrivyScan(repoName, image, ref)
				}
			}
		}

		// Format-native deletes: a successful DELETE to a repository (npm unpublish,
		// helm/maven/cran delete) or an OCI manifest DELETE removes an artifact.
		// Emit artifact.deleted centrally so every format is covered. (Admin-API
		// component deletes emit from their own handler with richer component/version
		// data and a different path prefix, so there is no double-fire.)
		if s.Webhooks != nil && r.Method == http.MethodDelete && status >= 200 && status < 300 {
			switch {
			case strings.HasPrefix(r.URL.Path, "/repository/"):
				repoName, subPath, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repository/"), "/")
				if repoName != "" {
					ev := webhook.Event{
						Type:      webhook.EventArtifactDeleted,
						Repo:      repoName,
						Path:      subPath,
						Actor:     actorLabel(r, s.Auth),
						Timestamp: start,
					}
					if rp, ok := s.Repos.Get(repoName); ok {
						ev.Format = rp.Format
					}
					go s.Webhooks.Dispatch(context.Background(), ev)
				}
			case strings.HasPrefix(r.URL.Path, "/v2/"):
				repoName, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
				if image, ref, ok := ociManifestRef(sub); ok && repoName != "" {
					go s.Webhooks.Dispatch(context.Background(), webhook.Event{
						Type:      webhook.EventArtifactDeleted,
						Repo:      repoName,
						Format:    "oci",
						Path:      image + ":" + ref,
						Actor:     actorLabel(r, s.Auth),
						Timestamp: start,
					})
				}
			}
		}

	})
}

// ociManifestRef extracts the image name and reference from an OCI sub-path of
// the form "{image}/manifests/{ref}" (the image may itself contain slashes).
// ok is false for blob, upload, and tag-list paths — those are not manifest
// operations and so are not artifact publishes/deletes.
func ociManifestRef(sub string) (image, ref string, ok bool) {
	const marker = "/manifests/"
	idx := strings.Index(sub, marker)
	if idx < 0 {
		return "", "", false
	}
	image, ref = sub[:idx], sub[idx+len(marker):]
	if image == "" || ref == "" || strings.Contains(ref, "/") {
		return "", "", false
	}
	return image, ref, true
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
