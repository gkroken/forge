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

	// Register one handler per format. This is the entire extension surface.
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())

	// Configure repositories. In production these come from a DB + admin API.
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "maven-hosted", Format: "maven", Kind: repo.Hosted},
		{Name: "maven-central", Format: "maven", Kind: repo.Proxy,
			Upstream: "https://repo1.maven.org/maven2"},
		{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted},
		{Name: "npm-proxy", Format: "npm", Kind: repo.Proxy,
			Upstream: "https://registry.npmjs.org"},
		{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted},
		{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy,
			Upstream: "https://cran.r-project.org"},
	} {
		must(mgr.Add(r))
	}

	srv := &http.Server{
		Addr:    *addr,
		Handler: server.New(mgr, reg, blobStore, metaStore).Routes(),
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
