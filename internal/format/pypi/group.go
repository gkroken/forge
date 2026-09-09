package pypi

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"forge/internal/format"
	"forge/internal/repo"
)

// Group mode: one index over several members, so a build can point at a single
// URL and get internal packages plus everything from pypi.org.
//
// The merge policy — member order, hosted shadowing proxy, claim filtering,
// dedupe — lives in format.GroupMerge, because none of it is about Python. All
// this file supplies is how to ask a member what it offers, and how to render
// the result as a PEP 503 page.

// serveGroup handles the read-only surface of a group repository.
func (h *Handler) serveGroup(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		http.Error(w, "group repositories are read-only: publish to the hosted member",
			http.StatusMethodNotAllowed)

	case c.Sub == "simple" || c.Sub == "simple/":
		h.groupIndex(w, c)

	case strings.HasPrefix(c.Sub, "simple/"):
		project := Normalize(strings.Trim(strings.TrimPrefix(c.Sub, "simple/"), "/"))
		if project == "" {
			http.NotFound(w, nil)
			return
		}
		h.groupSimpleProject(w, r, c, project)

	case strings.HasPrefix(c.Sub, "packages/"):
		// Whichever member holds the bytes answers; the members' own handlers
		// decide, so a hosted member reads its blobs and a proxy member fetches.
		if !format.GroupFetch(h, w, r, c) {
			http.NotFound(w, nil)
		}

	default:
		http.Error(w, "unsupported pypi request", http.StatusNotFound)
	}
}

// groupSimpleProject merges one project's files across every member.
func (h *Handler) groupSimpleProject(w http.ResponseWriter, r *http.Request, c *format.Context, project string) {
	files := format.GroupMerge(c,
		func(mc *format.Context) []upstreamFile { return h.memberFiles(mc, project) },
		func(f upstreamFile) (string, string) {
			// Two files of the same release (a wheel and an sdist) are distinct
			// entries, so the filename is what makes one link unique — keying on
			// the version alone would drop the sdist.
			return f.Project, f.Filename
		})
	if len(files) == 0 {
		http.Error(w, "no such project: "+project, http.StatusNotFound)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
	// Links point at the group, so pip keeps talking to the one URL it was given.
	renderSimplePage(w, r, c.Repo.Name, project, files)
}

// memberFiles asks one member what it offers for a project. A hosted member
// reads its own records; a proxy member has to consult upstream, because its
// cache holds only what has already been downloaded.
func (h *Handler) memberFiles(mc *format.Context, project string) []upstreamFile {
	if mc.Repo.Kind == repo.Proxy {
		files, err := h.proxyProjectFiles(mc, project)
		if err != nil {
			// An unreachable or 404-ing member must not fail the whole group:
			// the other members can still serve the project.
			return nil
		}
		return files
	}
	var out []upstreamFile
	for _, rec := range h.mustRecords(mc) {
		if rec.Project != project {
			continue
		}
		out = append(out, upstreamFile{
			Project:        rec.Project,
			Filename:       rec.Filename,
			SHA256:         rec.SHA256,
			RequiresPython: rec.RequiresPython,
		})
	}
	return out
}

// groupIndex lists the projects the group can enumerate: its hosted members'.
// A proxy member is skipped deliberately — pypi.org's root index is 45 MB of
// ~600k names (see proxy.go), and merging that per request would be unusable.
// Project pages still resolve for anything upstream has.
func (h *Handler) groupIndex(w http.ResponseWriter, c *format.Context) {
	seen := map[string]bool{}
	var projects []string
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok || mc.Repo.Kind == repo.Proxy {
			continue
		}
		for _, rec := range h.mustRecords(mc) {
			if !seen[rec.Project] {
				seen[rec.Project] = true
				projects = append(projects, rec.Project)
			}
		}
	}
	sort.Strings(projects)

	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Simple index</title></head><body>\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "<a href=\"%s/\">%s</a><br/>\n",
			template.HTMLEscapeString(p), template.HTMLEscapeString(p))
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String())) //nolint:errcheck
}
