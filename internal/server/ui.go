package server

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/format"
	"forge/internal/repo"
)

//go:embed templates static
var uiFS embed.FS

// cssVer is a short content-hash of style.css, computed once at startup.
// Injected into <link> URLs so browsers cache-bust on deploy.
var cssVer string

func init() {
	data, _ := fs.ReadFile(uiFS, "static/style.css")
	h := sha256.Sum256(data)
	cssVer = hex.EncodeToString(h[:4]) // 8 hex chars is plenty
}

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
	"urlPathEscape": url.PathEscape,
	"cssVer":        func() string { return cssVer },
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
	tmplLogin       = parseUITmpl("templates/base.html", "templates/login.html")
	tmplComponent   = parseUITmpl("templates/base.html", "templates/component.html")
	tmplTokens      = parseUITmpl("templates/base.html", "templates/tokens.html")
	tmplAccess      = parseUITmpl("templates/base.html", "templates/access.html")
	tmplUpload      = parseUITmpl("templates/base.html", "templates/upload.html")
)

// ── page data types ───────────────────────────────────────────────────────────

type componentPage struct {
	Title    string
	Repo     repo.Repository
	Name     string
	Versions []string
	RepoURL  string
}

type loginPage struct {
	Title       string
	Error       string
	Next        string
	OIDCEnabled bool
}

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
	Title      string
	Query      string
	Format     string
	Repo       string
	AllFormats []string
	AllRepos   []string
	Results    []searchResult
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
	case p == "/login":
		s.uiLogin(w, r)
	case p == "/logout":
		s.uiLogout(w, r)
	case strings.HasPrefix(p, "/repos/"):
		rest := strings.TrimPrefix(p, "/repos/")
		if rest == "" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		// /repos/{name} → repo detail
		// /repos/{name}/upload → upload page
		// /repos/{name}/{component} → component detail (strings.Cut on first "/" preserves @scope/pkg)
		repoName, sub, hasComponent := strings.Cut(rest, "/")
		if hasComponent && sub == "upload" {
			s.uiUpload(w, r, repoName)
		} else if hasComponent && sub != "" {
			s.uiComponent(w, r, repoName, sub)
		} else {
			s.uiRepo(w, r, repoName)
		}
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

func (s *Server) uiComponent(w http.ResponseWriter, r *http.Request, repoName, component string) {
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		http.NotFound(w, r)
		return
	}

	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, ok := h.(format.Browsable)
	if !ok {
		http.NotFound(w, r)
		return
	}

	entries, err := b.BrowseRepo(s.browseCtx(rp))
	if err != nil {
		http.Error(w, "browse error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var versions []string
	for _, e := range entries {
		if e.Name == component {
			versions = e.Versions
			break
		}
	}
	if versions == nil {
		http.NotFound(w, r)
		return
	}

	render(w, tmplComponent, "base.html", componentPage{
		Title:    component + " — " + repoName,
		Repo:     rp,
		Name:     component,
		Versions: versions,
		RepoURL:  publicBase(r) + "/repository/" + repoName + "/",
	})
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

	// Build repo list for the dropdown (all repos that support browsing).
	var allRepos []string
	for _, rp := range s.Repos.All() {
		if _, ok := s.Handlers.For(rp.Format); ok {
			allRepos = append(allRepos, rp.Name)
		}
	}

	data := searchPage{
		Title:      "Search",
		Query:      q,
		Format:     filterFormat,
		Repo:       filterRepo,
		AllFormats: allFormats,
		AllRepos:   allRepos,
		Results:    results,
	}
	// Boosted nav-bar requests (hx-boost) want the full page; only return the
	// partial fragment for direct htmx swap calls from the search page itself.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		render(w, tmplSearch, "search-results", data)
		return
	}
	render(w, tmplSearch, "base.html", data)
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	next := sanitizeNext(r.URL.Query().Get("next"))

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		secret := r.FormValue("token")
		if n := r.FormValue("next"); n != "" {
			next = sanitizeNext(n)
		}

		ok := s.verifyAdminSecret(secret)
		if !ok {
			render(w, tmplLogin, "base.html", loginPage{
				Title: "Sign in",
				Error: "Invalid token or insufficient permissions.",
				Next:  next,
			})
			return
		}
		http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteStrict already present
			Name:     auth.UISessionCookie,
			Value:    secret,
			Path:     "/",
			HttpOnly: true,
			Secure:   isSecureContext(r),
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, next, http.StatusSeeOther) // #nosec G710 -- next is always output of sanitizeNext(), which rejects absolute URLs
		return
	}

	errMsg := ""
	switch r.URL.Query().Get("error") {
	case "invalid":
		errMsg = "Invalid token or insufficient permissions."
	case "oidc":
		errMsg = "SSO login failed. Please try again or sign in with a token."
	}
	render(w, tmplLogin, "base.html", loginPage{
		Title:       "Sign in",
		Error:       errMsg,
		Next:        next,
		OIDCEnabled: s.OIDC != nil && s.Auth != nil,
	})
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- Secure set via isSecureContext; HttpOnly+SameSiteStrict already present
		Name:     auth.UISessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureContext(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// verifyAdminSecret returns true if secret is a valid admin token (or if auth
// is not enabled in eval mode).
func (s *Server) verifyAdminSecret(secret string) bool {
	if s.Auth == nil {
		return true
	}
	tok, err := s.Auth.Verify(secret)
	return err == nil && tok != nil && tok.RoleFor("*") >= auth.RoleAdmin
}

// sanitizeNext ensures the redirect target is a safe forge UI path,
// preventing open redirects to external URLs. It parses the URL, rejects
// absolute URLs, and strips any query/fragment so only the path is returned.
func sanitizeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/ui/") {
		return "/ui/admin/"
	}
	return u.Path
}

// isSecureContext reports whether the request arrived over TLS, either
// directly or via a reverse proxy that sets X-Forwarded-Proto.
func isSecureContext(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// ── helpers ───────────────────────────────────────────────────────────────────

// publicBase returns the scheme+host for the current request, respecting
// X-Forwarded-Proto set by reverse proxies.
func publicBase(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto + "://" + r.Host
	}
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

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
