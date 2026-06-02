package server

import (
	"net/http"
	"strings"
	"time"

	"forge/internal/repo"
)

var (
	allFormats = []string{"maven", "npm", "helm", "cran", "oci"}
	allKinds   = []string{"hosted", "proxy", "group"}
)

// ── page data types ───────────────────────────────────────────────────────────

type adminReposPage struct {
	Title string
	Repos []repo.Repository
	Flash string
}

type adminFormPage struct {
	Title   string
	Repo    repo.Repository
	KindStr string // string(Repo.Kind) — avoids named-type comparison in templates
	IsEdit  bool
	Error   string
	Formats []string
	Kinds   []string
}

// ── dispatcher ────────────────────────────────────────────────────────────────

// handleUIAdmin dispatches all /ui/admin/* routes.
func (s *Server) handleUIAdmin(w http.ResponseWriter, r *http.Request, sub string) {
	sub = strings.TrimRight(sub, "/")
	if sub == "" {
		sub = "/"
	}
	switch {
	case sub == "/" || sub == "":
		s.uiAdminHome(w, r)
	case sub == "/repos/new":
		s.uiAdminNewRepo(w, r)
	case strings.HasSuffix(sub, "/edit"):
		name := strings.TrimSuffix(strings.TrimPrefix(sub, "/repos/"), "/edit")
		s.uiAdminEditRepo(w, r, name)
	case strings.HasPrefix(sub, "/repos/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(sub, "/repos/")
		s.uiAdminDeleteRepo(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (s *Server) uiAdminHome(w http.ResponseWriter, r *http.Request) {
	render(w, tmplAdminRepos, "base.html", adminReposPage{
		Title: "Admin — Repositories",
		Repos: s.Repos.All(),
		Flash: r.URL.Query().Get("flash"),
	})
}

func (s *Server) uiAdminNewRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.processRepoForm(w, r, "", false)
		return
	}
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:   "Admin — New repository",
		Repo:    repo.Repository{Kind: repo.Hosted},
		KindStr: "hosted",
		Formats: allFormats,
		Kinds:   allKinds,
	})
}

func (s *Server) uiAdminEditRepo(w http.ResponseWriter, r *http.Request, name string) {
	rp, ok := s.Repos.Get(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.processRepoForm(w, r, name, true)
		return
	}
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:   "Admin — Edit " + name,
		Repo:    rp,
		KindStr: string(rp.Kind),
		IsEdit:  true,
		Formats: allFormats,
		Kinds:   allKinds,
	})
}

func (s *Server) uiAdminDeleteRepo(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.Repos.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// HX-Redirect triggers a full-page navigation in htmx; plain redirect for non-htmx.
	target := "/ui/admin/?flash=Deleted+repository+" + name
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther) // #nosec G710 -- target is a hardcoded /ui/admin/ prefix
}

// processRepoForm handles the POST for both create and edit.
func (s *Server) processRepoForm(w http.ResponseWriter, r *http.Request, existingName string, isEdit bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	if isEdit {
		name = existingName // URL name takes precedence
	}

	var ttl time.Duration
	if raw := strings.TrimSpace(r.FormValue("proxyTTL")); raw != "" {
		var err error
		ttl, err = time.ParseDuration(raw)
		if err != nil {
			s.reRenderForm(w, r, name, isEdit, "Invalid proxy TTL: "+err.Error())
			return
		}
	}

	var members []string
	if raw := strings.TrimSpace(r.FormValue("members")); raw != "" {
		for _, m := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(m); t != "" {
				members = append(members, t)
			}
		}
	}

	rp := repo.Repository{
		Name:          name,
		Format:        r.FormValue("format"),
		Kind:          repo.Kind(r.FormValue("kind")),
		Upstream:      strings.TrimSpace(r.FormValue("upstream")),
		Members:       members,
		AnonymousRead: r.FormValue("anonymousRead") == "on",
		ProxyAuth:     strings.TrimSpace(r.FormValue("proxyAuth")),
		ProxyTTL:      ttl,
	}

	if msg := validateRepo(rp); msg != "" {
		s.reRenderForm(w, r, existingName, isEdit, msg)
		return
	}

	var opErr error
	if isEdit {
		opErr = s.Repos.Update(rp)
	} else {
		opErr = s.Repos.Add(rp)
	}
	if opErr != nil {
		s.reRenderForm(w, r, existingName, isEdit, opErr.Error())
		return
	}

	action := "Created"
	if isEdit {
		action = "Updated"
	}
	http.Redirect(w, r, "/ui/admin/?flash="+action+"+repository+"+name, http.StatusSeeOther) // #nosec G710 -- target is a hardcoded /ui/admin/ prefix
}

func (s *Server) reRenderForm(w http.ResponseWriter, r *http.Request, name string, isEdit bool, errMsg string) {
	var rp repo.Repository
	if isEdit {
		rp, _ = s.Repos.Get(name)
	}
	// Overlay form values so the user doesn't lose their input.
	if v := r.FormValue("format"); v != "" {
		rp.Format = v
	}
	if v := r.FormValue("kind"); v != "" {
		rp.Kind = repo.Kind(v)
	}
	rp.Name = r.FormValue("name")
	if isEdit {
		rp.Name = name
	}
	rp.Upstream = r.FormValue("upstream")
	rp.ProxyAuth = r.FormValue("proxyAuth")
	rp.AnonymousRead = r.FormValue("anonymousRead") == "on"

	title := "Admin — New repository"
	if isEdit {
		title = "Admin — Edit " + name
	}
	render(w, tmplAdminForm, "base.html", adminFormPage{
		Title:   title,
		Repo:    rp,
		KindStr: string(rp.Kind),
		IsEdit:  isEdit,
		Error:   errMsg,
		Formats: allFormats,
		Kinds:   allKinds,
	})
}
