// Command forge is a prototype multi-format package repository manager.
//
// It demonstrates one shared spine (router -> repository manager -> blob +
// metadata stores) with pluggable per-format handlers for Maven, npm, Helm,
// and CRAN, each supporting hosted and (where applicable) proxy modes.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory")
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1")
	drainTimeout := flag.Duration("drain-timeout", 30*time.Second, "graceful shutdown drain timeout")
	enableAuth := flag.Bool("auth", false, "enable token authentication (creates token store in data dir)")
	logFormat := flag.String("log-format", "json", "log format: json or text")
	flag.Parse()

	obs.InitLog(*logFormat)

	if *healthcheck {
		resp, err := http.Get("http://127.0.0.1" + *addr + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	blobStore, err := blob.NewFS(*data + "/blobs")
	must(err)
	metaStore, err := meta.NewFS(*data + "/meta")
	must(err)

	// Auth store: nil = AllowAll (eval mode); non-nil = token enforcement.
	var authStore auth.Store
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
	}

	// Register one handler per format. This is the entire extension surface.
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())
	reg.Register(oci.New())

	// Configure repositories. In production these come from a DB + admin API.
	mgr := repo.NewManager()
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
			Upstream: "https://cran.r-project.org", AnonymousRead: true},
		// OCI / Docker
		{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted, AnonymousRead: !*enableAuth},
		// Group: merged read-only views (hosted first so internal artifacts shadow upstream).
		{Name: "maven-public", Format: "maven", Kind: repo.Group,
			Members: []string{"maven-hosted", "maven-central"}, AnonymousRead: true},
		{Name: "npm-public", Format: "npm", Kind: repo.Group,
			Members: []string{"npm-hosted", "npm-proxy"}, AnonymousRead: true},
		{Name: "helm-public", Format: "helm", Kind: repo.Group,
			Members: []string{"helm-hosted"}, AnonymousRead: true},
		{Name: "cran-public", Format: "cran", Kind: repo.Group,
			Members: []string{"cran-hosted", "cran-proxy"}, AnonymousRead: true},
	} {
		must(mgr.Add(r))
	}

	// Prometheus metrics — one registry per process.
	promReg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(promReg)

	// In eval mode (FS stores) use an in-memory queue; in production the caller
	// would pass a queue.NewPG(metaPG.DB()) here instead.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	q := queue.NewMem(256)

	srv := &http.Server{
		Addr: *addr,
		Handler: server.New(mgr, reg, blobStore, metaStore, authStore).
			WithMetrics(metrics, promReg).
			WithQueue(workerCtx, q).
			Routes(),
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
