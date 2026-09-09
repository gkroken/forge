package pypi

import (
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"forge/internal/format"
	"forge/internal/proxy"
	"forge/internal/repo"
)

// Proxy mode for pypi.org (or any PEP 503 index).
//
// Two upstream shapes have to be reconciled with forge's own URL space:
//
//   - The index lives on pypi.org, but the files it links to live on
//     files.pythonhosted.org. There is no single upstream prefix to append a
//     sub-path to, the way maven or cran can, so every link forge rewrites is
//     recorded as a project+filename → upstream URL mapping and the rewritten
//     link points back at forge.
//   - The root index (GET /simple/) is 45 MB of HTML naming ~600k projects,
//     with absolute hrefs that would point at forge's root rather than at this
//     repository. The shared fetcher buffers whole bodies in memory
//     (proxy.go, io.ReadAll), so proxying it would mean a 45 MB allocation per
//     miss. pip never reads it to install anything — it goes straight to the
//     project page — so it is refused with an explanation instead.

// upNS holds the project+filename → upstream URL mapping built while rewriting
// a proxied simple page. A file is fetchable only after its index page has been
// read, which also means forge never fetches a URL upstream did not hand it.
func (h *Handler) upNS(c *format.Context) string { return c.Repo.Name + ":pypi:up" }

// upKey addresses the mapping for one proxied file.
func upKey(project, filename string) string { return project + "/" + filename }

// upstreamFile is one link from a proxied simple page.
type upstreamFile struct {
	Project        string `json:"project"`
	Filename       string `json:"filename"`
	URL            string `json:"url"`
	SHA256         string `json:"sha256,omitempty"`
	RequiresPython string `json:"requiresPython,omitempty"`
	Yanked         string `json:"yanked,omitempty"`
	CoreMetadata   string `json:"coreMetadata,omitempty"`
}

// anchorRe matches one PEP 503 link. These pages are machine-generated with one
// anchor per line, so a tokenizer would buy nothing the attribute lookups below
// do not already give.
var (
	anchorRe = regexp.MustCompile(`(?is)<a\s+([^>]*?)>\s*([^<]*?)\s*</a>`)
	attrRe   = regexp.MustCompile(`(?is)([a-zA-Z][a-zA-Z0-9-]*)\s*=\s*"([^"]*)"`)
)

// attrs returns a tag's attributes with their values HTML-unescaped. Upstream
// serves them escaped ("&gt;=2.7"), and forge escapes again on the way out, so
// skipping this yields "&amp;gt;=2.7" — pip then reads the literal text
// "&gt;=2.7" as the requires-python specifier and cannot parse it.
func attrs(tag string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
		out[strings.ToLower(m[1])] = html.UnescapeString(m[2])
	}
	return out
}

// proxySimpleProject fetches a project's upstream simple page, records where
// each file really lives, and serves the page with every link pointing back at
// forge so pip downloads through the cache rather than around it.
// proxyProjectFiles fetches a project's upstream simple page, records where
// each file really lives, and returns the usable links. Split out from the
// rendering because a group repo needs the same records without the HTML.
//
// A proxy member's local cache is NOT the answer here: it holds only what has
// been downloaded so far, so a group built from it would hide versions the
// group can actually serve.
func (h *Handler) proxyProjectFiles(c *format.Context, project string) ([]upstreamFile, error) {
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/simple/" + url.PathEscape(project) + "/"
	key := c.Key("simple/" + project + "/index.html")

	// A simple page is the index: it gains a link on every upstream release.
	f := proxy.New(c.HTTP, c.ProxyMetadataConfig())
	rc, _, err := f.Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck

	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var out []upstreamFile
	for _, m := range anchorRe.FindAllStringSubmatch(string(body), -1) {
		a := attrs(m[1])
		if a["href"] == "" {
			continue
		}
		if rec, ok := h.mapUpstreamLink(c, project, a["href"], a); ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// renderSimplePage writes a PEP 503 page whose links point at repoName, which
// is the group when a group is serving and the proxy itself otherwise.
func renderSimplePage(w http.ResponseWriter, r *http.Request, repoName, project string, files []upstreamFile) {
	base := publicBase(r) + "/repository/" + repoName
	var b strings.Builder
	safeProject := template.HTMLEscapeString(project)
	fmt.Fprintf(&b, "<!DOCTYPE html><html><head><meta name=\"pypi:repository-version\" content=\"1.0\"><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", safeProject, safeProject)
	for _, rec := range files {
		fmt.Fprintf(&b, "<a href=\"%s\"", template.HTMLEscapeString(
			fmt.Sprintf("%s/packages/%s/%s#sha256=%s",
				base, url.PathEscape(project), url.PathEscape(rec.Filename), url.QueryEscape(rec.SHA256))))
		if rec.RequiresPython != "" {
			fmt.Fprintf(&b, " data-requires-python=\"%s\"", template.HTMLEscapeString(rec.RequiresPython))
		}
		if rec.Yanked != "" {
			fmt.Fprintf(&b, " data-yanked=\"%s\"", template.HTMLEscapeString(rec.Yanked))
		}
		// Kept, not dropped: pip asks for "{file}.metadata" when it sees this,
		// and proxyFile serves that sidecar from the same upstream URL. Keeping
		// the attribute without serving the sidecar would break resolution.
		if rec.CoreMetadata != "" {
			fmt.Fprintf(&b, " data-core-metadata=\"%s\"", template.HTMLEscapeString(rec.CoreMetadata))
		}
		fmt.Fprintf(&b, ">%s</a><br/>\n", template.HTMLEscapeString(rec.Filename))
	}
	b.WriteString("</body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(b.String())) //nolint:errcheck
}

// proxySimpleProject serves one project's page from upstream, rewritten so pip
// downloads through forge rather than around it.
func (h *Handler) proxySimpleProject(w http.ResponseWriter, r *http.Request, c *format.Context, project string) {
	files, err := h.proxyProjectFiles(c, project)
	if errors.Is(err, proxy.ErrNotFound) {
		http.Error(w, "no such project: "+project, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	renderSimplePage(w, r, c.Repo.Name, project, files)
}

// mapUpstreamLink records where one upstream file lives so a later request for
// it can be fetched. Links whose filename forge would refuse from a publisher
// are skipped rather than stored — upstream is not more trusted than a user.
func (h *Handler) mapUpstreamLink(c *format.Context, project, href string, a map[string]string) (upstreamFile, bool) {
	raw, frag, _ := strings.Cut(href, "#")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return upstreamFile{}, false
	}
	filename := path.Base(u.Path)
	if !validFilename(filename) {
		return upstreamFile{}, false
	}
	rec := upstreamFile{
		Project:        project,
		Filename:       filename,
		URL:            raw,
		RequiresPython: a["data-requires-python"],
		Yanked:         a["data-yanked"],
	}
	if v, ok := a["data-core-metadata"]; ok {
		rec.CoreMetadata = v
	} else if v, ok := a["data-dist-info-metadata"]; ok {
		rec.CoreMetadata = v
	}
	if sum, ok := strings.CutPrefix(frag, "sha256="); ok {
		rec.SHA256 = sum
	}
	c.Meta.PutJSON(h.upNS(c), upKey(project, filename), rec) //nolint:errcheck
	return rec, true
}

// proxyFile serves a file named by a previously-rewritten simple page, fetching
// and caching it from wherever upstream actually keeps it.
func (h *Handler) proxyFile(w http.ResponseWriter, c *format.Context, project, filename string) {
	// pip appends ".metadata" to a file URL when the index advertises
	// data-core-metadata; the sidecar lives beside the file upstream.
	lookup, metaSidecar := strings.CutSuffix(filename, ".metadata")

	var rec upstreamFile
	if ok, _ := c.Meta.GetJSON(h.upNS(c), upKey(project, lookup), &rec); !ok {
		// Nothing has read the index for this project yet, so forge has no
		// upstream URL for the file and will not guess one.
		http.NotFound(w, nil)
		return
	}

	upURL := rec.URL
	if metaSidecar {
		upURL += ".metadata"
	}
	f := proxy.New(c.HTTP, c.ProxyConfig())
	rc, ct, err := f.Fetch(c.Key("packages/"+project+"/"+filename), c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if errors.Is(err, proxy.ErrNotFound) {
		http.NotFound(w, nil)
		return
	}
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer rc.Close() //nolint:errcheck
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	io.Copy(w, rc) //nolint:errcheck
}

// cachedRecords describes what a proxy repo actually holds locally: the files
// pip has pulled through it, not the ~600k projects upstream offers. Browse,
// inspect and retention all read this, so a proxy's cache can be listed and
// evicted the same way hosted content is.
//
// The .metadata sidecars are skipped — they are an optimisation pip fetches
// beside a wheel, not a distribution anyone installs.
func (h *Handler) cachedRecords(c *format.Context) ([]fileRecord, error) {
	prefix := c.Repo.Name + "/packages/"
	keys, err := c.Blob.List(prefix)
	if err != nil {
		return nil, err
	}
	out := make([]fileRecord, 0, len(keys))
	for _, k := range keys {
		sub := strings.TrimPrefix(k, prefix)
		project, filename, ok := strings.Cut(sub, "/")
		if !ok || strings.HasSuffix(filename, ".metadata") {
			continue
		}
		version, ok := versionFromFilename(project, filename)
		if !ok {
			continue
		}
		rec := fileRecord{Project: project, Version: version, Filename: filename}
		if info, exists, _ := c.Blob.Stat(k); exists {
			rec.SHA256, rec.Size = info.SHA256, info.Size
		}
		// When the file was cached, which is the only "published at" a proxy
		// can honestly report.
		var entry proxy.CacheEntry
		if ok, _ := c.Meta.GetJSON(c.Repo.Name+":proxy", k, &entry); ok {
			rec.UploadedAt = entry.FetchedAt
		}
		var up upstreamFile
		if ok, _ := c.Meta.GetJSON(h.upNS(c), upKey(project, filename), &up); ok {
			rec.RequiresPython = up.RequiresPython
		}
		out = append(out, rec)
	}
	return out, nil
}

// deleteCached evicts one cached release from a proxy: the blobs, their cache
// entries, and the upstream mapping that would otherwise point at files no
// longer held.
func (h *Handler) deleteCached(c *format.Context, project, version string) (int64, error) {
	recs, err := h.cachedRecords(c)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, rec := range recs {
		if rec.Project != project || rec.Version != version {
			continue
		}
		for _, name := range []string{rec.Filename, rec.Filename + ".metadata"} {
			key := c.Key("packages/" + project + "/" + name)
			if info, exists, _ := c.Blob.Stat(key); exists {
				freed += info.Size
				c.Blob.Delete(key)                       //nolint:errcheck
				c.Meta.Delete(c.Repo.Name+":proxy", key) //nolint:errcheck
			}
		}
		c.Meta.Delete(h.upNS(c), upKey(project, rec.Filename)) //nolint:errcheck
	}
	return freed, nil
}

// proxyKind reports whether this context is a proxy repo, kept as one helper so
// the seams below read the same way.
func isProxy(c *format.Context) bool { return c.Repo.Kind == repo.Proxy }
