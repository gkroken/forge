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
	"forge/internal/config"
	"forge/internal/formats"
	"forge/internal/ldap"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/oidc"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/server"
	"forge/internal/trivy"
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

	// LDAP / Active Directory. Each flag defaults to its LDAP_* env var. Setting
	// -ldap-url enables direct AD/LDAP login on the web form (search-then-bind):
	// the user's AD password is verified against the directory and forge mints a
	// normal forge token — AD credentials never go into .npmrc/settings.xml/CI.
	ldapURL := flag.String("ldap-url", os.Getenv("LDAP_URL"), "LDAP server URL(s), comma-separated for failover, e.g. ldaps://dc1:636,ldap://dc2:389 — enables AD/LDAP login (env LDAP_URL)")
	ldapStartTLS := flag.Bool("ldap-start-tls", os.Getenv("LDAP_START_TLS") == "true", "issue StartTLS on ldap:// connections before binding (env LDAP_START_TLS)")
	ldapCACert := flag.String("ldap-ca-cert", os.Getenv("LDAP_CA_CERT"), "path to a PEM CA bundle for the LDAP TLS connection; default system roots (env LDAP_CA_CERT)")
	ldapInsecure := flag.Bool("ldap-insecure-skip-verify", os.Getenv("LDAP_INSECURE_SKIP_VERIFY") == "true", "disable LDAP TLS certificate verification — dev/test only (env LDAP_INSECURE_SKIP_VERIFY)")
	ldapBindDN := flag.String("ldap-bind-dn", os.Getenv("LDAP_BIND_DN"), "service-account DN for the search step; empty = anonymous search (env LDAP_BIND_DN)")
	ldapBindPassword := flag.String("ldap-bind-password", os.Getenv("LDAP_BIND_PASSWORD"), "service-account password — prefer the env var; flags are visible in ps (env LDAP_BIND_PASSWORD)")
	ldapUserBaseDN := flag.String("ldap-user-base-dn", os.Getenv("LDAP_USER_BASE_DN"), "base DN for the user search, e.g. ou=people,dc=example,dc=com (env LDAP_USER_BASE_DN)")
	ldapUserFilter := flag.String("ldap-user-filter", os.Getenv("LDAP_USER_FILTER"), "user search filter; %s = escaped login name; default (uid=%s); AD: (sAMAccountName=%s) (env LDAP_USER_FILTER)")
	ldapEmailAttr := flag.String("ldap-email-attr", os.Getenv("LDAP_EMAIL_ATTR"), "attribute holding the user's email, default \"mail\" (env LDAP_EMAIL_ATTR)")
	ldapGroupMode := flag.String("ldap-group-mode", os.Getenv("LDAP_GROUP_MODE"), "how to resolve groups: \"memberof\" (read attr off the user, AD default) or \"search\" (env LDAP_GROUP_MODE)")
	ldapGroupBaseDN := flag.String("ldap-group-base-dn", os.Getenv("LDAP_GROUP_BASE_DN"), "base DN for group search (group mode \"search\") (env LDAP_GROUP_BASE_DN)")
	ldapGroupFilter := flag.String("ldap-group-filter", os.Getenv("LDAP_GROUP_FILTER"), "group search filter; %s = escaped user DN, e.g. (&(objectClass=groupOfNames)(member=%s)) (env LDAP_GROUP_FILTER)")
	ldapGroupAttr := flag.String("ldap-group-attr", os.Getenv("LDAP_GROUP_ATTR"), "attribute holding the group name, default \"cn\" (env LDAP_GROUP_ATTR)")
	ldapGroupMappings := flag.String("ldap-group-mappings", os.Getenv("LDAP_GROUP_MAPPINGS"), "directory group→role map, e.g. forge-admins:admin,devs:write,staff:read (env LDAP_GROUP_MAPPINGS)")
	ldapTokenTTL := flag.String("ldap-token-ttl", os.Getenv("LDAP_TOKEN_TTL"), "lifetime of an LDAP session (default 8h) (env LDAP_TOKEN_TTL)")
	auditRetention := flag.String("audit-retention", os.Getenv("AUDIT_RETENTION"), "how long to keep Postgres audit_log entries, e.g. 2160h (default 90d); 0 disables pruning (env AUDIT_RETENTION)")
	trashRetention := flag.String("trash-retention", envOr("TRASH_RETENTION", "168h"), "how long soft-deleted artifacts stay in trash before purge, e.g. 168h (default 7d); 0 keeps them until manually purged (env TRASH_RETENTION)")
	// Trivy OCI image scanning. Setting -trivy-addr enables the sidecar scanner;
	// Trivy must be reachable at -trivy-binary (default: found in PATH).
	// Each flag defaults to its TRIVY_* env var so either works.
	trivyBinary := flag.String("trivy-binary", envOr("TRIVY_BINARY", "trivy"), "path to the trivy binary (env TRIVY_BINARY)")
	trivyAddr := flag.String("trivy-addr", os.Getenv("TRIVY_ADDR"), "forge registry address for Trivy image pulls, e.g. localhost:8080; setting this enables OCI scanning (env TRIVY_ADDR)")
	trivyAuthToken := flag.String("trivy-auth-token", os.Getenv("TRIVY_AUTH_TOKEN"), "forge API token for Trivy registry auth; empty = no auth (env TRIVY_AUTH_TOKEN)")

	// Declarative config-as-code. When -config is set, forge reads the file on
	// every boot (JSON, or YAML when the extension is .yaml/.yml) and
	// reconciles repos/policies/roles/webhooks to match.
	// Secrets are injected via ${ENV_VAR} placeholders — never commit them.
	// env FORGE_CONFIG overrides the default (empty = eval mode, seeds used).
	configPath := flag.String("config", envOr("FORGE_CONFIG", ""), "path to forge.config.{json,yaml}; enables config-as-code mode (env FORGE_CONFIG)")
	configCheck := flag.Bool("config-check", false, "validate -config file and print plan, then exit 0 (valid) / 1 (invalid)")
	configExport := flag.Bool("config-export", false, "print current state as a config file to stdout and exit 0")
	configExportFormat := flag.String("config-export-format", "json", "format for -config-export: json | yaml")
	configAdopt := flag.Bool("config-adopt", false, "allow -config to take ownership of pre-existing objects whose settings differ (overwrites them; each is audited)")
	configOverride := flag.Bool("allow-config-override", false, "break-glass: permit admin API/UI writes to config-managed objects (each is logged and audited; the next apply reverts them)")
	configDriftEvery := flag.Duration("config-drift-interval", time.Minute, "how often to refresh the config-drift gauge in -config mode (0 disables the background check)")
	configWatch := flag.Bool("config-watch", false, "re-apply the -config file when its contents change, without a restart (opt-in; default is to converge on boot only)")
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
				auth.GrantForRole("*", auth.RoleAdmin),
			}, nil)
			must(err)
			slog.Info("auth enabled: bootstrap admin token created", "id", tok.ID, "secret", secret)
			slog.Warn("store the bootstrap secret; it will not be shown again")
		}
		userStore = auth.NewUserStore(metaStore)
		roleStore = auth.NewRoleStore(metaStore)
	}

	// Register one handler per format. This is the entire extension surface.
	reg := formats.Registry()

	// Repository manager: load persisted repos from the meta store, then seed
	// defaults on first run (when the store is empty). Skipped when -config is
	// set — the config file is the source of truth in that mode.
	mgr := repo.NewManager()
	must(mgr.WithStore(metaStore))

	if *configPath == "" {
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
		WithFormats(reg).
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

	var trivyScanner *trivy.Scanner
	if *trivyAddr != "" {
		trivyScanner = trivy.New(*trivyBinary, *trivyAddr, *trivyAuthToken)
		slog.Info("trivy: OCI image scanning enabled", "addr", *trivyAddr, "binary", *trivyBinary)
	}

	// Config-as-code reconcile / export / check. All three modes need the
	// same Appliers (built from the stores above). Only one mode runs.
	cfgAppliers := config.Appliers{
		Repos:    mgr,
		Cleanup:  cleanupPolicies,
		Vuln:     vulnPolicies,
		Roles:    roleStore,
		Webhooks: webhookEngine.Store(),
		Meta:     metaStore,
		Audit:    auditSink,
	}
	if *configExport {
		asYAML := false
		switch strings.ToLower(*configExportFormat) {
		case "yaml", "yml":
			asYAML = true
		case "json":
		default:
			slog.Error("-config-export-format must be json or yaml", "got", *configExportFormat)
			os.Exit(1)
		}
		f, err := config.Export(cfgAppliers)
		must(err)
		out, err := config.Marshal(f, asYAML)
		must(err)
		if !asYAML {
			out = append(out, '\n') // match the trailing newline json.Encoder wrote
		}
		_, err = os.Stdout.Write(out)
		must(err)
		os.Exit(0)
	}
	// ldapFromConfig, when set by a -config file's ldap block, overrides the
	// -ldap-* flags at wiring time below.
	var ldapFromConfig *ldap.Config
	if *configPath != "" || *configCheck {
		if *configPath == "" {
			slog.Error("-config-check requires -config <path>")
			os.Exit(1)
		}
		f, err := config.Load(*configPath)
		if err != nil {
			slog.Error("config: load failed", "err", err)
			os.Exit(1)
		}
		// -config-adopt is the CLI spelling of the file's "adopt" key; either
		// one permits taking ownership of differing unmanaged objects.
		f.Adopt = f.Adopt || *configAdopt
		if *configCheck {
			res, err := config.Plan(f, cfgAppliers)
			if err != nil {
				slog.Error("config: validation failed", "err", err)
				os.Exit(1)
			}
			slog.Info("config plan",
				"repos_create", res.Repositories.Created,
				"repos_update", res.Repositories.Updated,
				"repos_noop", res.Repositories.Noop,
				"repos_delete", res.Repositories.Deleted,
				"repos_adopt", res.Repositories.Adopted,
				"cleanup_create", res.CleanupPolicies.Created,
				"cleanup_update", res.CleanupPolicies.Updated,
				"cleanup_adopt", res.CleanupPolicies.Adopted,
				"vuln_create", res.SecurityPolicies.Created,
				"vuln_update", res.SecurityPolicies.Updated,
				"vuln_adopt", res.SecurityPolicies.Adopted,
				"roles_create", res.Roles.Created,
				"roles_update", res.Roles.Updated,
				"roles_adopt", res.Roles.Adopted,
				"webhooks_create", res.Webhooks.Created,
				"webhooks_update", res.Webhooks.Updated,
				"webhooks_adopt", res.Webhooks.Adopted,
				"conflicts", len(res.Conflicts),
				"security_default_set", res.SecurityDefaultSet,
				"ldap_configured", res.LDAPConfigured,
			)
			// Conflicts are objects this file has never managed whose settings
			// differ. Without adopt they would block Apply, so -config-check must
			// report the file as not-applyable.
			for _, c := range res.Conflicts {
				if f.Adopt {
					slog.Warn("config: will force-adopt", "object", c.String())
				} else {
					slog.Error("config: adoption conflict", "object", c.String())
				}
			}
			if len(res.Conflicts) > 0 && !f.Adopt {
				slog.Error("config: not applyable without -config-adopt (or \"adopt\": true)",
					"conflicts", len(res.Conflicts))
				os.Exit(1)
			}
			os.Exit(0)
		}
		// -config mode: apply on boot (fatal on error — bad desired state must
		// stop a rollout before traffic reaches the pod).
		res, err := config.Apply(f, cfgAppliers)
		must(err)
		ldapFromConfig = f.LDAP // consumed at the LDAP wiring block below
		adopted := res.Repositories.Adopted + res.CleanupPolicies.Adopted +
			res.SecurityPolicies.Adopted + res.Roles.Adopted + res.Webhooks.Adopted
		slog.Info("config applied",
			"repos", res.Repositories.Changes(),
			"cleanup_policies", res.CleanupPolicies.Changes(),
			"security_policies", res.SecurityPolicies.Changes(),
			"roles", res.Roles.Changes(),
			"webhooks", res.Webhooks.Changes(),
			"adopted", adopted,
			"forced_adoptions", len(res.Conflicts),
		)
		// A forced adoption overwrote state somebody set outside the config file.
		// It is audited, but it should also be loud in the boot log.
		for _, c := range res.Conflicts {
			slog.Warn("config: force-adopted unmanaged object", "object", c.String())
		}
	}

	// configPlanner re-reads the file on every call, so a ConfigMap update is
	// reflected in drift without a restart.
	var configPlanner func() (config.Result, error)
	if *configPath != "" {
		path, adopt := *configPath, *configAdopt
		configPlanner = func() (config.Result, error) {
			f, err := config.Load(path)
			if err != nil {
				return config.Result{}, err
			}
			f.Adopt = f.Adopt || adopt
			return config.Plan(f, cfgAppliers)
		}
	}

	forgeSrv := server.New(mgr, reg, blobStore, metaStore, authStore).
		WithConfigMode(*configPath, *configOverride).
		WithConfigDrift(configPlanner).
		WithMetrics(metrics, promReg).
		WithGlobalStats(globalStats).
		WithWebhooks(webhookEngine).
		WithVuln(vulnStore, osvClient).
		WithVulnPolicy(vulnPolicies).
		WithTrivy(trivyScanner).
		WithQueue(workerCtx, q).
		WithCleanup(cleanupPolicies).
		WithScheduler(cleanupScheduler).
		WithAuditLog(auditSink).
		WithUsers(userStore).
		WithRoles(roleStore).
		WithBlobWalker(workerCtx)

	// Soft-delete trash retention (default 7d; 0 keeps trash until manually purged).
	if d, err := time.ParseDuration(*trashRetention); err != nil {
		slog.Error("invalid -trash-retention", "value", *trashRetention, "err", err)
		os.Exit(1)
	} else {
		forgeSrv.WithTrashRetention(d)
	}

	// Register the daily vuln re-scan and the trash retention sweep as scheduler
	// tick hooks (leader-gated, shared lastRun). Both run inside the single hook.
	cleanupScheduler.WithTickHook(func(now time.Time, lastRun map[string]time.Time) {
		forgeSrv.VulnRescanTick(now, lastRun)
		forgeSrv.TrashPurgeTick(now, lastRun)
	})
	cleanupScheduler.Start(workerCtx)

	if *oidcIssuer != "" {
		grants := []auth.Grant{auth.GrantForRole("*", auth.RoleRead)}
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

	// LDAP config source: a -config file's ldap block wins over the -ldap-* flags.
	var ldapCfg *ldap.Config
	switch {
	case ldapFromConfig != nil:
		ldapCfg = ldapFromConfig
	case *ldapURL != "":
		grants := []auth.Grant{auth.GrantForRole("*", auth.RoleRead)}
		if raw := os.Getenv("LDAP_DEFAULT_GRANTS"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &grants); err != nil {
				slog.Error("ldap: invalid LDAP_DEFAULT_GRANTS", "err", err)
				os.Exit(1)
			}
		}
		ttl := 8 * time.Hour
		if *ldapTokenTTL != "" {
			d, err := time.ParseDuration(*ldapTokenTTL)
			if err != nil {
				slog.Error("ldap: invalid -ldap-token-ttl", "err", err)
				os.Exit(1)
			}
			ttl = d
		}
		mappings, err := auth.ParseGroupMappings(*ldapGroupMappings)
		if err != nil {
			slog.Error("ldap: invalid -ldap-group-mappings", "err", err)
			os.Exit(1)
		}
		ldapCfg = &ldap.Config{
			URLs:               splitComma(*ldapURL),
			StartTLS:           *ldapStartTLS,
			CACertFile:         *ldapCACert,
			InsecureSkipVerify: *ldapInsecure,
			BindDN:             *ldapBindDN,
			BindPassword:       *ldapBindPassword,
			UserBaseDN:         *ldapUserBaseDN,
			UserFilter:         *ldapUserFilter,
			EmailAttr:          *ldapEmailAttr,
			GroupMode:          *ldapGroupMode,
			GroupBaseDN:        *ldapGroupBaseDN,
			GroupFilter:        *ldapGroupFilter,
			GroupAttr:          *ldapGroupAttr,
			GroupMappings:      mappings,
			DefaultGrants:      grants,
			TokenTTL:           ttl,
		}
	}
	if ldapCfg != nil {
		client, err := ldap.New(*ldapCfg)
		if err != nil {
			slog.Error("ldap: invalid configuration", "err", err)
			os.Exit(1)
		}
		forgeSrv = forgeSrv.WithLDAP(client, auth.NewGroupRoleMapper(ldapCfg.GroupMappings))
		if ldapCfg.InsecureSkipVerify {
			slog.Warn("ldap: TLS certificate verification DISABLED (InsecureSkipVerify) — do not use in production")
		}
		slog.Info("ldap: configured", "servers", len(ldapCfg.URLs),
			"group_mode", client.GroupMode(), "group_rules", len(ldapCfg.GroupMappings))
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: forgeSrv.Routes(),
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	// Keep the drift gauge current so Prometheus/Argo see divergence without
	// anyone hitting the endpoint.
	driftDone := make(chan struct{})
	defer close(driftDone)
	if *configDriftEvery > 0 {
		forgeSrv.StartDriftWatcher(*configDriftEvery, driftDone)
	}
	// Opt-in continuous reconcile. Triggers on the FILE changing, never on live
	// state changing — reverting an operator's edit on a timer is self-heal,
	// which forge deliberately does not do (the write is refused at the door).
	if *configWatch && *configPath != "" {
		every := *configDriftEvery
		if every <= 0 {
			every = time.Minute
		}
		go config.Watch(*configPath, cfgAppliers, every, driftDone, func(res config.Result, err error) {
			if err != nil {
				slog.Error("config: re-apply failed", "err", err)
				return
			}
			slog.Info("config: re-applied after file change",
				"repos", res.Repositories.Changes(),
				"cleanup_policies", res.CleanupPolicies.Changes(),
				"security_policies", res.SecurityPolicies.Changes(),
				"roles", res.Roles.Changes(),
				"webhooks", res.Webhooks.Changes(),
			)
		})
	}
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

// envOr returns the value of the env var name, or fallback when unset/empty.
// splitComma splits a comma-separated list, trimming blanks — used for the
// failover-ordered LDAP server list.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
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
