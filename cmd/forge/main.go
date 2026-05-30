// Command forge is a prototype multi-format package repository manager.
//
// It demonstrates one shared spine (router -> repository manager -> blob +
// metadata stores) with pluggable per-format handlers for Maven, npm, Helm,
// and CRAN, each supporting hosted and (where applicable) proxy modes.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/repo"
	"forge/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory")
	healthcheck := flag.Bool("healthcheck", false, "probe /healthz and exit 0/1")
	drainTimeout := flag.Duration("drain-timeout", 30*time.Second, "graceful shutdown drain timeout")
	enableAuth := flag.Bool("auth", false, "enable token authentication (creates token store in data dir)")
	flag.Parse()

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
			log.Printf("forge: auth enabled — bootstrap admin token created")
			log.Printf("forge: token id=%s secret=%s", tok.ID, secret)
			log.Printf("forge: store this secret; it will not be shown again")
		}
	}

	// Register one handler per format. This is the entire extension surface.
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())

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

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(mgr, reg, blobStore, metaStore, authStore).Routes(),
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Println("forge: draining in-flight requests...")
		ctx, cancel := context.WithTimeout(context.Background(), *drainTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("forge: shutdown error: %v", err)
		}
	}()

	log.Printf("forge listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("forge: stopped")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
