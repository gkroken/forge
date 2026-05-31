package server

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"forge/internal/format"
	"forge/internal/repo"
)

//go:embed templates static
var uiFS embed.FS

var uiFuncs = template.FuncMap{
	"join": strings.Join,
	"add":  func(a, b int) int { return a + b },
	"sub":  func(a, b int) int { return a - b },
	"slice3": func(ss []string) []string {
		if len(ss) <= 3 {
			return ss
		}
		return ss[:3]
	},
	"durStr": func(d time.Duration) string {
		if d == 0 {
			return ""
		}
		return d.String()
	},
}

func parseUITmpl(files ...string) *template.Template {
	return template.Must(template.New("").Funcs(uiFuncs).ParseFS(uiFS, files...))
}

var (
	tmplHome        = parseUITmpl("templates/base.html", "templates/home.html")
	tmplRepo        = parseUITmpl("templates/base.html", "templates/repo.html")
	tmplSearch      = parseUITmpl("templates/base.html", "templates/search.html")
	tmplAdminRepos  = parseUITmpl("templates/base.html", "templates/admin_repos.html")
	tmplAdminForm   = parseUITmpl("templates/base.html", "templates/admin_repo_form.html")
)

// ── page data types ───────────────────────────────────────────────────────────

type homePage struct {
	Title string
	Repos []repoRow
}

type repoRow struct {
	Name   string
	Format string
	Kind   string
	Count  int // -1 = browse not supported for this format
}

type repoPage struct {
	Title      string
	Repo       repo.Repository
	Components []componentItem
	Total      int
	Page       int
	Limit      int
	HasMore    bool
	Query      string
}

type searchPage struct {
	Title   string
	Query   string
	Results []searchResult
}

// ── dispatcher ────────────────────────────────────────────────────────────────

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/ui")
	p = strings.TrimRight(p, "/")
	if p == "" {
		p = "/"
	}
	switch {
	case p == "/":
		s.uiHome(w, r)
	case p == "/search":
		s.uiSearch(w, r)
	case strings.HasPrefix(p, "/repos/"):
		name := strings.TrimPrefix(p, "/repos/")
		if name == "" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		s.uiRepo(w, r, name)
	case p == "/admin" || strings.HasPrefix(p, "/admin/"):
		s.handleUIAdmin(w, r, strings.TrimPrefix(p, "/admin"))
	default:
		http.NotFound(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (s *Server) uiHome(w http.ResponseWriter, r *http.Request) {
	var rows []repoRow
	for _, rp := range s.Repos.All() {
		count := -1
		if h, ok := s.Handlers.For(rp.Format); ok {
			if b, ok := h.(format.Browsable); ok {
				c := s.browseCtx(rp)
				if entries, err := b.BrowseRepo(c); err == nil {
					count = len(entries)
				}
			}
		}
		rows = append(rows, repoRow{
			Name: rp.Name, Format: rp.Format,
			Kind: string(rp.Kind), Count: count,
		})
	}
	render(w, tmplHome, "base.html", homePage{Title: "Repositories", Repos: rows})
}

func (s *Server) uiRepo(w http.ResponseWriter, r *http.Request, name string) {
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query().Get("q")
	page := clampedInt(r, "page", 1, 1, 1<<20)
	const limit = 50

	var components []componentItem
	var total int

	if h, ok := s.Handlers.For(rp.Format); ok {
		if b, ok := h.(format.Browsable); ok {
			if entries, err := b.BrowseRepo(s.browseCtx(rp)); err == nil {
				if q != "" {
					ql := strings.ToLower(q)
					kept := entries[:0]
					for _, e := range entries {
						if strings.Contains(strings.ToLower(e.Name), ql) {
							kept = append(kept, e)
						}
					}
					entries = kept
				}
				total = len(entries)
				start := (page - 1) * limit
				if start < total {
					end := start + limit
					if end > total {
						end = total
					}
					for _, e := range entries[start:end] {
						components = append(components, componentItem{
							Name: e.Name, Versions: e.Versions,
						})
					}
				}
			}
		}
	}

	data := repoPage{
		Title: rp.Name, Repo: rp,
		Components: components, Total: total,
		Page: page, Limit: limit,
		HasMore: page*limit < total,
		Query:   q,
	}
	if r.Header.Get("HX-Request") == "true" {
		render(w, tmplRepo, "components-section", data)
		return
	}
	render(w, tmplRepo, "base.html", data)
}

func (s *Server) uiSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	filterFormat := r.URL.Query().Get("format")
	filterRepo := r.URL.Query().Get("repo")

	var results []searchResult
	if ql := strings.ToLower(strings.TrimSpace(q)); ql != "" {
		for _, rp := range s.Repos.All() {
			if filterFormat != "" && rp.Format != filterFormat {
				continue
			}
			if filterRepo != "" && rp.Name != filterRepo {
				continue
			}
			h, ok := s.Handlers.For(rp.Format)
			if !ok {
				continue
			}
			b, ok := h.(format.Browsable)
			if !ok {
				continue
			}
			entries, err := b.BrowseRepo(s.browseCtx(rp))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Name), ql) {
					results = append(results, searchResult{
						Repo: rp.Name, Format: rp.Format,
						Name: e.Name, Versions: e.Versions,
					})
				}
			}
		}
	}
	if results == nil {
		results = []searchResult{}
	}

	data := searchPage{Title: "Search", Query: q, Results: results}
	if r.Header.Get("HX-Request") == "true" {
		render(w, tmplSearch, "search-results", data)
		return
	}
	render(w, tmplSearch, "base.html", data)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// browseCtx builds a format.Context suitable for BrowseRepo calls (no Sub/Queue).
func (s *Server) browseCtx(rp repo.Repository) *format.Context {
	return &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta,
		HTTP: s.client, Repos: s.Repos, Metrics: s.Metrics,
	}
}

// render executes a named template into a buffer, then writes it to w.
// Buffering ensures a clean 500 if template rendering fails mid-output.
func render(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w) //nolint:errcheck
}

// serveUIStatic returns a handler for /ui/static/ backed by the embedded FS.
func (s *Server) serveUIStatic() http.Handler {
	sub, _ := fs.Sub(uiFS, "static")
	return http.StripPrefix("/ui/static/", http.FileServer(http.FS(sub)))
}
