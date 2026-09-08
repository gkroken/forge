// Package pypi implements the Python package format.
//
// Two protocols, both small:
//
//	POST /                         -> twine upload (multipart, :action=file_upload)
//	GET  /simple/                  -> PEP 503 index of every project
//	GET  /simple/{project}/        -> PEP 503 links to that project's files
//	GET  /packages/{filename}      -> the artifact itself
//
// Hosted and proxy are both implemented; see proxy.go for how a proxied index
// is rewritten and why the upstream root index is refused. Group is not
// implemented, and says so with a 501 rather than serving an empty index.
//
// Naming is the part that bites. PEP 503 says "Foo.Bar", "foo-bar" and
// "foo_bar" are the same project, so every identity forge keeps — the meta
// namespace, the ledger, dependency-confusion claims, OSV lookups — uses the
// normalized form, and only filenames keep their original spelling.
package pypi

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/ledger"
	"forge/internal/repo"
)

type Handler struct{ format.Unsupported }

func New() *Handler               { return &Handler{} }
func (h *Handler) Format() string { return "pypi" }

// fileRecord is one uploaded artifact: a wheel or an sdist.
type fileRecord struct {
	Project    string    `json:"project"` // normalized
	Version    string    `json:"version"`
	Filename   string    `json:"filename"` // original spelling
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploadedAt,omitempty"`
	// RequiresPython is served as the data-requires-python attribute so pip can
	// skip files it cannot install.
	RequiresPython string `json:"requiresPython,omitempty"`
}

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + ":pypi" }

// recordKey is unique per artifact: a release usually has both a wheel and an
// sdist, and may have several wheels for different platforms.
func recordKey(project, version, filename string) string {
	return project + "/" + version + "/" + filename
}

func (h *Handler) fileKey(c *format.Context, project, filename string) string {
	return c.Key("packages/" + project + "/" + filename)
}

// --- PEP 503 naming ---------------------------------------------------------

var normalizeRe = regexp.MustCompile(`[-_.]+`)

// Normalize applies PEP 503: runs of "-", "_" and "." collapse to a single "-",
// and the result is lowercased. "Foo.Bar", "foo-bar" and "foo__bar" are all
// "foo-bar", and forge must agree with pip about that or a package published
// under one spelling is invisible when requested by another.
func Normalize(name string) string {
	return strings.ToLower(normalizeRe.ReplaceAllString(name, "-"))
}

// nameRe is PEP 508's project-name rule; versionRe and filenameRe are the
// character sets PyPI itself accepts. Anything outside them is rejected at
// upload rather than stored, because these values are interpolated into the
// simple pages that browsers render — a filename like `x"><img onerror=…>` is
// stored XSS against everyone who later browses the repository. Output is
// escaped as well (see simpleIndex/simpleProject); this is the outer of the two
// layers, and the one that keeps bad values out of the store in the first place.
var (
	nameRe     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	versionRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.!+_-]*$`)
	filenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

// validFilename also insists on an extension pip understands, so the store
// cannot fill up with things no client will ever install.
func validFilename(name string) bool {
	if !filenameRe.MatchString(name) {
		return false
	}
	return strings.HasSuffix(name, ".whl") ||
		strings.HasSuffix(name, ".tar.gz") ||
		strings.HasSuffix(name, ".zip") ||
		strings.HasSuffix(name, ".egg")
}

// --- Serve ------------------------------------------------------------------

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	// Group is the one kind with no path here yet. Said up front, because the
	// alternative is answering /simple/ with an empty but perfectly valid
	// index: pip resolves nothing and reports only "no matching distribution",
	// with no hint that the repository kind is the cause.
	if c.Repo.Kind == repo.Group {
		http.Error(w, "pypi groups are not implemented: use the hosted or proxy repository directly",
			http.StatusNotImplemented)
		return
	}

	if c.Repo.Kind == repo.Proxy {
		h.serveProxy(w, r, c)
		return
	}

	switch {
	case r.Method == http.MethodPost && (c.Sub == "" || c.Sub == "/"):
		h.upload(w, r, c)

	case r.Method == http.MethodGet && (c.Sub == "simple" || c.Sub == "simple/"):
		h.simpleIndex(w, c)

	case r.Method == http.MethodGet && strings.HasPrefix(c.Sub, "simple/"):
		project := Normalize(strings.Trim(strings.TrimPrefix(c.Sub, "simple/"), "/"))
		h.simpleProject(w, r, c, project)

	case r.Method == http.MethodGet && strings.HasPrefix(c.Sub, "packages/"):
		h.download(w, c)

	case r.Method == http.MethodDelete && strings.HasPrefix(c.Sub, "packages/"):
		h.deleteFile(w, c)

	default:
		http.Error(w, "unsupported pypi request", http.StatusNotFound)
	}
}

// serveProxy handles the read-only surface of a proxy repository. Publishing
// and deleting belong to the hosted repo the proxy caches from, not here.
func (h *Handler) serveProxy(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		http.Error(w, "proxy repositories are read-only", http.StatusMethodNotAllowed)

	case c.Sub == "simple" || c.Sub == "simple/":
		// The upstream root index is 45 MB naming ~600k projects, and the
		// shared fetcher buffers whole bodies in memory. pip never reads it to
		// install anything, so refusing beats an allocation that size per miss.
		http.Error(w, "the root simple index is not proxied — request a project directly, e.g. /simple/requests/",
			http.StatusNotImplemented)

	case strings.HasPrefix(c.Sub, "simple/"):
		project := Normalize(strings.Trim(strings.TrimPrefix(c.Sub, "simple/"), "/"))
		if project == "" {
			http.NotFound(w, nil)
			return
		}
		h.proxySimpleProject(w, r, c, project)

	case strings.HasPrefix(c.Sub, "packages/"):
		project, filename, ok := artifactPath(c.Sub)
		if !ok {
			http.NotFound(w, nil)
			return
		}
		h.proxyFile(w, c, project, filename)

	default:
		http.Error(w, "unsupported pypi request", http.StatusNotFound)
	}
}

// --- upload (twine) ---------------------------------------------------------

// upload accepts twine's multipart form. twine sends the distribution metadata
// as form fields alongside the file, so nothing has to be parsed out of the
// archive itself.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, c *format.Context) {
	// #nosec G120 -- the body is already bounded: the spine wraps every write
	// method in http.MaxBytesReader (internal/server/server.go, -max-upload).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if action := r.FormValue(":action"); action != "" && action != "file_upload" {
		http.Error(w, "unsupported :action "+action, http.StatusBadRequest)
		return
	}
	name, version := r.FormValue("name"), r.FormValue("version")
	if name == "" || version == "" {
		http.Error(w, "name and version are required", http.StatusBadRequest)
		return
	}
	if !nameRe.MatchString(name) {
		http.Error(w, "invalid project name", http.StatusBadRequest)
		return
	}
	if !versionRe.MatchString(version) {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("content")
	if err != nil {
		http.Error(w, "missing file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close() //nolint:errcheck

	body, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filename := path.Base(header.Filename)
	if !validFilename(filename) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	project := Normalize(name)
	info, err := c.Blob.Put(h.fileKey(c, project, filename), bytes.NewReader(body))
	if err != nil {
		if errors.Is(err, blob.ErrImmutable) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rec := fileRecord{
		Project: project, Version: version, Filename: filename,
		SHA256: info.SHA256, Size: info.Size,
		UploadedAt:     time.Now().UTC(),
		RequiresPython: r.FormValue("requires_python"),
	}
	c.Meta.PutJSON(h.ns(c), recordKey(project, version, filename), rec) //nolint:errcheck
	ledger.RecordAt(c.Meta, c.Repo.Name, project, version, rec.UploadedAt)

	// Explicit content type so nothing here is ever sniffed as markup; the
	// values are validated above, and twine treats any 2xx as success.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- not a markup context: all three values are validated
	// against the character sets above (no "<", ">" or quotes survive), the
	// response is explicitly text/plain, and the spine sends nosniff
	// (internal/server/server.go). Taint analysis cannot see any of that.
	fmt.Fprintf(w, "stored %s %s (%s)\n", project, version, filename)
}

// --- simple index (PEP 503) -------------------------------------------------

func (h *Handler) simpleIndex(w http.ResponseWriter, c *format.Context) {
	recs, err := h.records(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	seen := map[string]bool{}
	var projects []string
	for _, rec := range recs {
		if !seen[rec.Project] {
			seen[rec.Project] = true
			projects = append(projects, rec.Project)
		}
	}
	sort.Strings(projects)

	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Simple index</title></head><body>\n")
	for _, p := range projects {
		fmt.Fprintf(&b, "<a href=\"%s/\">%s</a><br/>\n",
			template.HTMLEscapeString(url.PathEscape(p)), template.HTMLEscapeString(p))
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String())) //nolint:errcheck
}

func (h *Handler) simpleProject(w http.ResponseWriter, r *http.Request, c *format.Context, project string) {
	recs, err := h.records(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var files []fileRecord
	for _, rec := range recs {
		if rec.Project == project {
			files = append(files, rec)
		}
	}
	if len(files) == 0 {
		http.Error(w, "no such project: "+project, http.StatusNotFound)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })

	base := publicBase(r) + "/repository/" + c.Repo.Name
	var b strings.Builder
	safeProject := template.HTMLEscapeString(project)
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", safeProject, safeProject)
	for _, f := range files {
		href := fmt.Sprintf("%s/packages/%s/%s#sha256=%s",
			base, url.PathEscape(project), url.PathEscape(f.Filename), url.QueryEscape(f.SHA256))
		if f.RequiresPython != "" {
			fmt.Fprintf(&b, "<a href=\"%s\" data-requires-python=\"%s\">%s</a><br/>\n",
				template.HTMLEscapeString(href), template.HTMLEscapeString(f.RequiresPython),
				template.HTMLEscapeString(f.Filename))
		} else {
			fmt.Fprintf(&b, "<a href=\"%s\">%s</a><br/>\n",
				template.HTMLEscapeString(href), template.HTMLEscapeString(f.Filename))
		}
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String())) //nolint:errcheck
}

// --- download / delete ------------------------------------------------------

func (h *Handler) download(w http.ResponseWriter, c *format.Context) {
	rc, err := c.Blob.Get(c.Key(c.Sub))
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	defer rc.Close() //nolint:errcheck
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, rc) //nolint:errcheck
}

func (h *Handler) deleteFile(w http.ResponseWriter, c *format.Context) {
	key := c.Key(c.Sub)
	if _, exists, _ := c.Blob.Stat(key); !exists {
		http.NotFound(w, nil)
		return
	}
	if err := c.Blob.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Drop the record for this file, and the ledger entry once the release has
	// no files left.
	project, filename := projectAndFile(strings.TrimPrefix(c.Sub, "packages/"))
	for _, rec := range h.mustRecords(c) {
		if rec.Project == project && rec.Filename == filename {
			c.Meta.Delete(h.ns(c), recordKey(rec.Project, rec.Version, rec.Filename)) //nolint:errcheck
			if !h.releaseHasFiles(c, rec.Project, rec.Version) {
				ledger.Forget(c.Meta, c.Repo.Name, rec.Project, rec.Version)
			}
			break
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func projectAndFile(sub string) (project, filename string) {
	project, filename, _ = strings.Cut(sub, "/")
	return project, filename
}

func (h *Handler) releaseHasFiles(c *format.Context, project, version string) bool {
	for _, rec := range h.mustRecords(c) {
		if rec.Project == project && rec.Version == version {
			return true
		}
	}
	return false
}

// --- record access ----------------------------------------------------------

// records is the one source every seam reads. A hosted repo owns its records; a
// proxy has none of its own and answers with what it has actually cached, so
// browse, inspect and retention describe a proxy honestly instead of empty.
func (h *Handler) records(c *format.Context) ([]fileRecord, error) {
	if isProxy(c) {
		return h.cachedRecords(c)
	}
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	out := make([]fileRecord, 0, len(keys))
	for _, k := range keys {
		var rec fileRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (h *Handler) mustRecords(c *format.Context) []fileRecord {
	recs, _ := h.records(c)
	return recs
}

// publicBase reconstructs the externally visible base URL, so links in a simple
// page point back at forge rather than at whatever upstream said.
func publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}
