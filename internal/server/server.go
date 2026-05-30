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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

type Server struct {
	Repos    *repo.Manager
	Handlers *format.Registry
	Blob     blob.Store
	Meta     meta.Store
	client   *http.Client
}

func New(m *repo.Manager, reg *format.Registry, b blob.Store, mt meta.Store) *Server {
	return &Server{
		Repos: m, Handlers: reg, Blob: b, Meta: mt,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/repository/", s.handleRepo)
	mux.HandleFunc("/", s.handleIndex)
	return logging(mux)
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
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		fmt.Printf("%s %s (%s)\n", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
