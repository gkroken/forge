// Command forge is a prototype multi-format package repository manager.
//
// It demonstrates one shared spine (router -> repository manager -> blob +
// metadata stores) with pluggable per-format handlers for Maven, npm, Helm,
// and CRAN, each supporting hosted and (where applicable) proxy modes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/oidc"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/server"
	"forge/internal/vuln"
	"forge/internal/webhook"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory")
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1")
	drainTimeout := flag.Duration("drain-timeout", 30*time.Second, "graceful shutdown drain timeout")
	enableAuth := flag.Bool("auth", false, "enable token authentication (creates token store in data dir)")
	logFormat := flag.String("log-format", "json", "log format: json or text")

	// OIDC / SSO. Each flag defaults to its OIDC_* env var so either works; a
	// flag value overrides the env. Setting the issuer enables SSO. Works with
	// any OIDC IdP — Keycloak, Entra/Azure AD, Okta, ADFS — which is how forge
	// integrates with Active Directory (the IdP brokers AD).
	oidcIssuer := flag.String("oidc-issuer", os.Getenv("OIDC_ISSUER"), "OIDC issuer URL — enables SSO (env OIDC_ISSUER)")
	oidcClientID := flag.String("oidc-client-id", os.Getenv("OIDC_CLIENT_ID"), "OIDC client ID (env OIDC_CLIENT_ID)")
	oidcClientSecret := flag.String("oidc-client-secret", os.Getenv("OIDC_CLIENT_SECRET"), "OIDC client secret — prefer the env var; flags are visible in ps (env OIDC_CLIENT_SECRET)")
	oidcRedirectURL := flag.String("oidc-redirect-url", os.Getenv("OIDC_REDIRECT_URL"), "OIDC redirect URL, e.g. https://forge.example.com/auth/oidc/callback (env OIDC_REDIRECT_URL)")
	oidcGroupsClaim := flag.String("oidc-groups-claim", os.Getenv("OIDC_GROUPS_CLAIM"), "ID-token claim holding group membership (default \"groups\") (env OIDC_GROUPS_CLAIM)")
	oidcGroupMappings := flag.String("oidc-group-mappings", os.Getenv("OIDC_GROUP_MAPPINGS"), "IdP group→role map, e.g. forge-admins:admin,devs:write,staff:read (env OIDC_GROUP_MAPPINGS)")
	oidcTokenTTL := flag.String("oidc-token-ttl", os.Getenv("OIDC_TOKEN_TTL"), "lifetime of an SSO session (default 8h) (env OIDC_TOKEN_TTL)")
	auditRetention := flag.String("audit-retention", os.Getenv("AUDIT_RETENTION"), "how long to keep Postgres audit_log entries, e.g. 2160h (default 90d); 0 disables pruning (env AUDIT_RETENTION)")
	flag.Parse()

	obs.InitLog(*logFormat)

	if *healthcheck {
		resp, err := http.Get("http://127.0.0.1" + *addr + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Storage backend selection.
	// External mode: set POSTGRES_DSN + S3_ENDPOINT + S3_BUCKET env vars.
	// Eval mode (default): filesystem under -data directory, zero external deps.
	var (
		blobStore blob.Store
		metaStore meta.Store
		pgMeta    *meta.PG // non-nil when Postgres is active; used for queue wiring
		err       error
	)

	if pgDSN := os.Getenv("POSTGRES_DSN"); pgDSN != "" {
		pgMeta, err = meta.NewPG(pgDSN)
		must(err)
		metaStore = pgMeta
		slog.Info("meta store: postgres", "dsn", redactDSN(pgDSN))
	} else {
		metaStore, err = meta.NewFS(*data + "/meta")
		must(err)
	}

	if s3Ep := os.Getenv("S3_ENDPOINT"); s3Ep != "" {
		blobStore, err = blob.NewS3(blob.S3Config{
			Endpoint:  s3Ep,
			Bucket:    os.Getenv("S3_BUCKET"),
			AccessKey: os.Getenv("S3_ACCESS_KEY"),
			SecretKey: os.Getenv("S3_SECRET_KEY"),
			UseSSL:    os.Getenv("S3_USE_SSL") == "true",
		})
		must(err)
		slog.Info("blob store: s3", "endpoint", s3Ep, "bucket", os.Getenv("S3_BUCKET"))
	} else {
		blobStore, err = blob.NewFS(*data + "/blobs")
		must(err)
	}

	// Auth store: nil = AllowAll (eval mode); non-nil = token enforcement.
	var (
		authStore auth.Store
		userStore auth.UserStore
		roleStore auth.RoleStore
	)
	if *enableAuth {
		authStore = auth.NewMetaStore(metaStore)
		n, err := authStore.Count()
		must(err)
		if n == 0 {
			tok, secret, err := authStore.Create("bootstrap admin", []auth.Grant{
				{Repo: "*", Role: auth.RoleAdmin},
			}, nil)
			must(err)
			slog.Info("auth enabled: bootstrap admin token created", "id", tok.ID, "secret", secret)
			slog.Warn("store the bootstrap secret; it will not be shown again")
		}
		userStore = auth.NewUserStore(metaStore)
		roleStore = auth.NewRoleStore(metaStore)
	}

	// Register one handler per format. This is the entire extension surface.
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())
	reg.Register(oci.New())

	// Repository manager: load persisted repos from the meta store, then seed
	// defaults on first run (when the store is empty).
	mgr := repo.NewManager()
	must(mgr.WithStore(metaStore))

	if mgr.Len() == 0 {
		slog.Info("seeding default repositories")
	}
	for _, r := range []repo.Repository{
		// Hosted repos: writes always require a token; reads require one too
		// unless auth is disabled (eval mode) or AnonymousRead is set.
		// Hosted: source of truth for internal artifacts.
		{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		// Proxy: read-through caches of public registries.
		{Name: "maven-central", Format: "maven", Kind: repo.Proxy,
			Upstream: "https://repo1.maven.org/maven2", AnonymousRead: true},
		{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy,
			Upstream: "https://registry.npmjs.org", AnonymousRead: true},
		{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy,
			Upstream: cranProxyUpstream(), AnonymousRead: true},
		{Name: "helm-proxy", Format: "helm", Kind: repo.Proxy,
			Upstream: "https://charts.bitnami.com/bitnami", AnonymousRead: true},
		// OCI / Docker
		{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		// Group: merged read-only views (hosted first so internal artifacts shadow upstream).
		{Name: "maven-public", Format: "maven", Kind: repo.Group,
			Members: []string{"maven-hosted", "maven-central"}, AnonymousRead: true},
		{Name: "npm-public", Format: "npm", Kind: repo.Group,
			Members: []string{"npm-hosted", "npm-proxy"}, AnonymousRead: true},
		{Name: "helm-public", Format: "helm", Kind: repo.Group,
			Members: []string{"helm-hosted", "helm-proxy"}, AnonymousRead: true},
		{Name: "cran-public", Format: "cran", Kind: repo.Group,
			Members: []string{"cran-hosted", "cran-proxy"}, AnonymousRead: true},
	} {
		// Seeded repos start online. Enabled has no "unset" sentinel, so the
		// struct literals above leave it false; set it here before persisting
		// or a fresh data dir comes up with every repo offline (503).
		r.Enabled = true
		// Add only if not already persisted (idempotent first-run seeding).
		if err := mgr.Add(r); err != nil {
			slog.Debug("skipping repo seed (already exists)", "name", r.Name)
		}
	}

	// Global stats collector — wraps metaStore to capture latency EMA.
	globalStats := obs.NewGlobalStats()
	metaStore = obs.NewLatencyStore(metaStore, globalStats.MetaLatencyMS)

	// Prometheus metrics — one registry per process.
	promReg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(promReg)

	// Use the Postgres queue when a PG meta store is active so that index-regen
	// jobs survive pod restarts and are shared across all app nodes. Fall back to
	// the in-memory queue for eval / single-node mode (FS stores).
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	var q queue.Queue
	if pgMeta != nil {
		q = queue.NewPG(pgMeta.DB())
		slog.Info("queue: postgres")
	} else {
		q = queue.NewMem(256)
		slog.Info("queue: in-memory (eval mode)")
	}

	// Webhooks: durable on-publish delivery via the shared queue. Construct
	// before WithQueue so its delivery handler is registered before the worker
	// starts, and before Routes() so the publish hook can emit events.
	// SSRF guard: deny webhook targets on loopback/link-local/private/metadata
	// ranges (validated at create/update AND at dial time to defeat rebinding),
	// unless WEBHOOK_ALLOW_PRIVATE is set for internal-only deployments.
	webhookGuard := webhook.NewSSRFGuard(envTrue("WEBHOOK_ALLOW_PRIVATE"))
	webhookEngine := webhook.New(metaStore, q, webhookGuard.HTTPClient(10*time.Second)).
		WithSSRFGuard(webhookGuard).
		WithMetrics(func(result string) {
			metrics.WebhookDeliveries.WithLabelValues(result).Inc()
		})

	// Vulnerability scanning: source-agnostic findings store + OSV producer
	// (npm + Maven). Construct before WithQueue so the scan handler is registered
	// before the worker starts, and before Routes() so the publish hook enqueues
	// scans. Pure stdlib HTTP against OSV.dev (no key, no new dependency).
	vulnStore := vuln.NewStore(metaStore)
	osvClient := vuln.NewClient(&http.Client{Timeout: 20 * time.Second})
	vulnPolicies := vuln.NewPolicyManager(metaStore)

	cleanupPolicies := cleanup.NewPolicyManager(metaStore)
	cleanupScheduler := cleanup.NewScheduler(mgr, cleanupPolicies, blobStore, metaStore).
		// Emit a cleanup.completed webhook after an automated run removes artifacts.
		WithRunHook(func(ev cleanup.RunEvent) {
			webhookEngine.EmitCleanupCompleted(context.Background(),
				ev.Repo, ev.Policy, ev.Deleted, ev.FreedBytes, ev.Trigger)
		})
	// In multi-replica (Postgres) mode, gate scheduled cleanup behind a Postgres
	// advisory lock with shared lastRun so a due job fires exactly once across
	// pods. Eval / single-node (FS) mode keeps the in-memory single-node behavior.
	if pgMeta != nil {
		cleanupScheduler.WithCoordinator(cleanup.NewPGCoordinator(pgMeta.DB()))
		slog.Info("cleanup scheduler: postgres-coordinated (advisory lock)")
	} else {
		slog.Info("cleanup scheduler: in-memory (eval mode)")
	}
	// Started below, after the server is built, so the vuln daily re-scan tick
	// hook can be registered (it needs the server's handlers + queue).

	// Audit log: Postgres-backed when PG is active (durable + coherent across
	// replicas), else the in-memory ring buffer for eval / single-node mode.
	var auditSink obs.AuditSink
	if pgMeta != nil {
		retention := 90 * 24 * time.Hour
		if *auditRetention != "" {
			d, err := time.ParseDuration(*auditRetention)
			must(err)
			retention = d
		}
		auditSink = obs.NewPGAuditSink(workerCtx, pgMeta.DB(), retention)
		slog.Info("audit log: postgres", "retention", retention)
	} else {
		auditSink = obs.NewAuditLog(500)
		slog.Info("audit log: in-memory (eval mode)")
	}

	forgeSrv := server.New(mgr, reg, blobStore, metaStore, authStore).
		WithMetrics(metrics, promReg).
		WithGlobalStats(globalStats).
		WithWebhooks(webhookEngine).
		WithVuln(vulnStore, osvClient).
		WithVulnPolicy(vulnPolicies).
		WithQueue(workerCtx, q).
		WithCleanup(cleanupPolicies).
		WithScheduler(cleanupScheduler).
		WithAuditLog(auditSink).
		WithUsers(userStore).
		WithRoles(roleStore).
		WithBlobWalker(workerCtx)

	// Register the daily vuln re-scan as a scheduler tick hook (leader-gated,
	// shared lastRun) and start the scheduler now that the server exists.
	cleanupScheduler.WithTickHook(forgeSrv.VulnRescanTick)
	cleanupScheduler.Start(workerCtx)

	if *oidcIssuer != "" {
		grants := []auth.Grant{{Repo: "*", Role: auth.RoleRead}}
		if raw := os.Getenv("OIDC_DEFAULT_GRANTS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &grants); err != nil {
				slog.Error("oidc: invalid OIDC_DEFAULT_GRANTS", "err", err)
				os.Exit(1)
			}
		}
		ttl := 8 * time.Hour
		if *oidcTokenTTL != "" {
			d, err := time.ParseDuration(*oidcTokenTTL)
			if err != nil {
				slog.Error("oidc: invalid -oidc-token-ttl", "err", err)
				os.Exit(1)
			}
			ttl = d
		}
		mappings, err := oidc.ParseGroupMappings(*oidcGroupMappings)
		if err != nil {
			slog.Error("oidc: invalid -oidc-group-mappings", "err", err)
			os.Exit(1)
		}
		groupsClaim := *oidcGroupsClaim
		if groupsClaim == "" {
			groupsClaim = "groups" // default; keeps the startup log and Access panel accurate
		}
		cfg := oidc.Config{
			Issuer: *oidcIssuer, ClientID: *oidcClientID, ClientSecret: *oidcClientSecret,
			RedirectURL: *oidcRedirectURL, GroupsClaim: groupsClaim,
			GroupMappings: mappings, DefaultGrants: grants, TokenTTL: ttl,
		}
		if err := cfg.Validate(); err != nil {
			slog.Error("oidc: invalid configuration", "err", err)
			os.Exit(1)
		}
		provider, err := oidc.New(context.Background(), cfg)
		if err != nil {
			slog.Error("oidc: provider discovery failed", "err", err)
			os.Exit(1)
		}
		forgeSrv = forgeSrv.WithOIDC(provider, auth.NewGroupRoleMapper(mappings))
		slog.Info("oidc: configured", "issuer", cfg.Issuer,
			"groups_claim", cfg.GroupsClaim, "group_rules", len(mappings))
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: forgeSrv.Routes(),
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		slog.Info("draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
		workerCancel()
	}()

	slog.Info("forge listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	slog.Info("forge stopped")
}

func must(err error) {
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// envTrue reports whether an env var is set to a truthy value (1/true/yes/on).
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// cranProxyUpstream returns the upstream URL for cran-proxy. CRAN_PROXY_UPSTREAM
// overrides the default so conformance tests can point at a local mock server.
func cranProxyUpstream() string {
	if u := os.Getenv("CRAN_PROXY_UPSTREAM"); u != "" {
		return u
	}
	return "https://cran.r-project.org"
}

// redactDSN strips the password from a postgres DSN for safe logging.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	u.User = url.User(u.User.Username())
	return u.String()
}
