// Package helm implements a Helm chart repository (classic HTTP, ChartMuseum-style).
//
// Endpoints:
//
//	GET  /index.yaml              -> generated index of every chart version held
//	GET  /{name}-{version}.tgz    -> chart download
//	POST /api/charts             -> upload a chart (.tgz body)
//	GET  /api/charts             -> JSON: all charts
//	GET  /api/charts/{name}      -> JSON: versions of one chart
//	DELETE /api/charts/{name}/{version}
//
// Group: read-only fan-out. index.yaml merges all members (dedup by name+version,
// first member wins); chart downloads try each member in order.
//
// OCI-based `helm push` is a separate protocol layered on a Docker registry -
// noted as a TODO.
package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"forge/internal/format"
	"forge/internal/proxy"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "helm" }

// chartRecord is what we persist per chart version (meta namespace).
type chartRecord struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	AppVersion  string    `json:"appVersion,omitempty"`
	Description string    `json:"description,omitempty"`
	Digest      string    `json:"digest"`
	Created     string    `json:"created"`
	Filename    string    `json:"filename"`
	UploadedAt  time.Time `json:"uploadedAt,omitempty"`
}

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + ":helm" }

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch {
	case r.Method == http.MethodGet && c.Sub == "index.yaml":
		h.index(w, c)
	case r.Method == http.MethodGet && c.Sub == "api/charts":
		h.listAll(w, c)
	case r.Method == http.MethodGet && strings.HasPrefix(c.Sub, "api/charts/"):
		h.listOne(w, c, strings.TrimPrefix(c.Sub, "api/charts/"))
	case r.Method == http.MethodPost && c.Sub == "api/charts":
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.upload(w, r, c)
	case r.Method == http.MethodDelete && strings.HasPrefix(c.Sub, "api/charts/"):
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot delete from non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.delete(w, c, strings.TrimPrefix(c.Sub, "api/charts/"))
	case r.Method == http.MethodGet && strings.HasSuffix(c.Sub, ".tgz"):
		h.download(w, c)
	default:
		http.Error(w, "unsupported helm request", http.StatusNotFound)
	}
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request, c *format.Context) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta, err := parseChartYAML(body)
	if err != nil {
		http.Error(w, "invalid chart: "+err.Error(), http.StatusBadRequest)
		return
	}
	filename := fmt.Sprintf("%s-%s.tgz", meta.Name, meta.Version)
	info, err := c.Blob.Put(c.Key(filename), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	rec := chartRecord{
		Name: meta.Name, Version: meta.Version, AppVersion: meta.AppVersion,
		Description: meta.Description, Digest: info.SHA256,
		Created: now.Format(time.RFC3339), Filename: filename,
		UploadedAt: now,
	}
	if err := c.Meta.PutJSON(h.ns(c), meta.Name+"-"+meta.Version, rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]bool{"saved": true})
}

func (h *Handler) download(w http.ResponseWriter, c *format.Context) {
	if c.Repo.Kind == repo.Group {
		h.groupDownload(w, c)
		return
	}
	rc, err := c.Blob.Get(c.Key(path.Base(c.Sub)))
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, rc)
}

func (h *Handler) groupDownload(w http.ResponseWriter, c *format.Context) {
	filename := path.Base(c.Sub)
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		rc, err := mc.Blob.Get(mc.Key(filename))
		if err != nil {
			continue
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/gzip")
		io.Copy(w, rc)
		return
	}
	http.NotFound(w, nil)
}

func (h *Handler) records(c *format.Context) []chartRecord {
	keys, _ := c.Meta.List(h.ns(c))
	var recs []chartRecord
	for _, k := range keys {
		var rec chartRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); ok {
			recs = append(recs, rec)
		}
	}
	return recs
}

// upstreamRecords fetches the upstream index.yaml for a proxy repo, caches it,
// and parses it into chartRecord slices. Returns nil on any error.
func (h *Handler) upstreamRecords(c *format.Context) []chartRecord {
	key := c.Key("index.yaml")
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/index.yaml"
	cfg := proxy.ConfigForRepo(c.Repo)
	f := proxy.New(c.HTTP, cfg)
	rc, _, err := f.Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return parseIndexYAML(data)
}

// parseIndexYAML parses a Helm index.yaml into chartRecord slices using a
// line-by-line state machine. No YAML library is used.
//
// Two common indentation styles are supported:
//   - Helm CLI: entry dash at indent 4, fields at indent 6
//   - Bitnami/ChartMuseum: entry dash at indent 2, fields at indent 4
//
// The parser discovers entryDashIndent from the first dash encountered and
// derives fieldIndent = entryDashIndent + 2.
func parseIndexYAML(data []byte) []chartRecord {
	var recs []chartRecord
	cur := chartRecord{}
	hasCur := false

	inEntries := false
	entryDashIndent := -1 // set when first entry dash is found
	inURLs := false
	contKey := ""  // field key whose value continues on wrapped lines
	contVal := ""  // accumulated value for contKey

	flushCont := func() {
		if contKey != "" {
			applyField(&cur, contKey+": "+contVal)
			contKey, contVal = "", ""
		}
	}

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)

		if !inEntries {
			if trimmed == "entries:" {
				inEntries = true
			}
			continue
		}

		// Back to top level (e.g. "generated:")
		if indent == 0 {
			flushCont()
			if hasCur {
				recs = append(recs, cur)
				cur = chartRecord{}
				hasCur = false
			}
			inEntries = false
			continue
		}

		// Chart name key at indent 2 (no dash, ends with ":")
		if indent == 2 && !strings.HasPrefix(trimmed, "-") {
			flushCont()
			if hasCur {
				recs = append(recs, cur)
				cur = chartRecord{}
				hasCur = false
			}
			entryDashIndent = -1
			inURLs = false
			continue
		}

		// New version entry: a "- " at the entry-dash indent level
		if strings.HasPrefix(trimmed, "- ") && (entryDashIndent == -1 || indent == entryDashIndent) {
			flushCont()
			if hasCur {
				recs = append(recs, cur)
			}
			cur = chartRecord{}
			hasCur = true
			entryDashIndent = indent
			inURLs = false
			// Apply any inline key-value (e.g. "- name: foo")
			applyField(&cur, strings.TrimPrefix(trimmed, "- "))
			continue
		}

		if !hasCur || entryDashIndent < 0 {
			continue
		}
		fieldIndent := entryDashIndent + 2

		// Continuation line: deeper indent, no dash, while a field is pending
		if indent > fieldIndent && contKey != "" && !strings.HasPrefix(trimmed, "-") {
			contVal += " " + trimmed
			continue
		}

		// Lines at the field level
		if indent == fieldIndent {
			flushCont()
			if strings.HasPrefix(trimmed, "- ") {
				// List item at field level (deps, maintainers, keywords, etc. or urls item)
				if inURLs && cur.Filename == "" {
					cur.Filename = strings.TrimPrefix(trimmed, "- ")
				}
				continue
			}
			// Plain key at field level — check if it starts a nested block to skip
			key, val, hasVal := strings.Cut(trimmed, ": ")
			if !hasVal {
				key = strings.TrimSuffix(trimmed, ":")
			}
			switch key {
			case "urls":
				inURLs = true
			case "annotations", "dependencies", "maintainers", "keywords", "sources":
				inURLs = false
			default:
				inURLs = false
				if hasVal {
					// Stash as potentially-continued field
					contKey = key
					contVal = strings.TrimSpace(strings.Trim(val, `"'`))
				}
			}
			continue
		}

		// Deeper lines: only care about url items
		if indent > fieldIndent && inURLs && strings.HasPrefix(trimmed, "- ") {
			if cur.Filename == "" {
				cur.Filename = strings.TrimPrefix(trimmed, "- ")
			}
		}
	}
	flushCont()
	if hasCur {
		recs = append(recs, cur)
	}

	// Populate UploadedAt from the Created field.
	for i := range recs {
		if recs[i].Created != "" {
			if t, err := time.Parse(time.RFC3339, recs[i].Created); err == nil {
				recs[i].UploadedAt = t
			} else if t, err := time.Parse(time.RFC3339Nano, recs[i].Created); err == nil {
				recs[i].UploadedAt = t
			}
		}
	}
	return recs
}

func applyField(rec *chartRecord, kv string) {
	idx := strings.Index(kv, ": ")
	if idx < 0 {
		return
	}
	k := strings.TrimSpace(kv[:idx])
	v := strings.TrimSpace(strings.Trim(strings.TrimSpace(kv[idx+2:]), `"'`))
	switch strings.TrimSpace(k) {
	case "name":
		rec.Name = v
	case "version":
		rec.Version = v
	case "appVersion":
		rec.AppVersion = v
	case "description":
		rec.Description = v
	case "created":
		rec.Created = v
	case "digest":
		rec.Digest = v
	}
}

// groupRecords merges chart records from all members, deduplicating by
// name+version (first member with a given name+version wins).
func (h *Handler) groupRecords(c *format.Context) []chartRecord {
	seen := map[string]bool{}
	var all []chartRecord
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		var recs []chartRecord
		if mc.Repo.Kind == repo.Proxy {
			recs = h.upstreamRecords(mc)
		} else {
			recs = h.records(mc)
		}
		for _, rec := range recs {
			key := rec.Name + "-" + rec.Version
			if !seen[key] {
				seen[key] = true
				all = append(all, rec)
			}
		}
	}
	return all
}

// index emits a valid Helm index.yaml grouped by chart name.
func (h *Handler) index(w http.ResponseWriter, c *format.Context) {
	var recs []chartRecord
	if c.Repo.Kind == repo.Group {
		recs = h.groupRecords(c)
	} else {
		recs = h.records(c)
	}
	w.Header().Set("Content-Type", "application/yaml")
	io.WriteString(w, buildIndex(recs, time.Now().UTC()))
}

// buildIndex is the pure generator for index.yaml, accepting an explicit now
// so tests can produce deterministic output.
func buildIndex(recs []chartRecord, now time.Time) string {
	byName := map[string][]chartRecord{}
	for _, rec := range recs {
		byName[rec.Name] = append(byName[rec.Name], rec)
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %s:\n", n)
		vers := byName[n]
		sort.Slice(vers, func(i, j int) bool { return vers[i].Version > vers[j].Version })
		for _, rec := range vers {
			fmt.Fprintf(&b, "    - apiVersion: v2\n      name: %s\n      version: %s\n",
				rec.Name, rec.Version)
			if rec.AppVersion != "" {
				fmt.Fprintf(&b, "      appVersion: %q\n", rec.AppVersion)
			}
			if rec.Description != "" {
				fmt.Fprintf(&b, "      description: %s\n", rec.Description)
			}
			fmt.Fprintf(&b, "      created: %s\n      digest: %s\n      urls:\n        - %s\n",
				rec.Created, rec.Digest, rec.Filename)
		}
	}
	fmt.Fprintf(&b, "generated: %s\n", now.Format(time.RFC3339))
	return b.String()
}

func (h *Handler) listAll(w http.ResponseWriter, c *format.Context) {
	var recs []chartRecord
	if c.Repo.Kind == repo.Group {
		recs = h.groupRecords(c)
	} else {
		recs = h.records(c)
	}
	byName := map[string][]chartRecord{}
	for _, rec := range recs {
		byName[rec.Name] = append(byName[rec.Name], rec)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(byName)
}

func (h *Handler) listOne(w http.ResponseWriter, c *format.Context, name string) {
	var source []chartRecord
	if c.Repo.Kind == repo.Group {
		source = h.groupRecords(c)
	} else {
		source = h.records(c)
	}
	var out []chartRecord
	for _, rec := range source {
		if rec.Name == name {
			out = append(out, rec)
		}
	}
	if out == nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *Handler) delete(w http.ResponseWriter, c *format.Context, nameVer string) {
	name, ver, ok := strings.Cut(nameVer, "/")
	if !ok {
		http.Error(w, "expected name/version", http.StatusBadRequest)
		return
	}
	c.Meta.Delete(h.ns(c), name+"-"+ver)
	c.Blob.Delete(c.Key(fmt.Sprintf("%s-%s.tgz", name, ver)))
	w.WriteHeader(http.StatusOK)
}

// --- minimal Chart.yaml extraction --------------------------------------

type chartMeta struct{ Name, Version, AppVersion, Description string }

// parseChartYAML pulls the top-level scalar fields we need out of the
// Chart.yaml inside a chart .tgz. A real implementation would use a YAML
// library; chart metadata top-level fields are simple scalars so a line scan
// is sufficient for the prototype.
func parseChartYAML(tgz []byte) (chartMeta, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return chartMeta{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return chartMeta{}, err
		}
		if path.Base(hdr.Name) == "Chart.yaml" {
			data, _ := io.ReadAll(tr)
			return scanChartYAML(data), nil
		}
	}
	return chartMeta{}, fmt.Errorf("Chart.yaml not found in archive")
}

func scanChartYAML(data []byte) chartMeta {
	var m chartMeta
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue // only top-level keys
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(strings.Trim(strings.TrimSpace(val), `"'`))
		switch strings.TrimSpace(key) {
		case "name":
			m.Name = val
		case "version":
			m.Version = val
		case "appVersion":
			m.AppVersion = val
		case "description":
			m.Description = val
		}
	}
	return m
}

// BrowseRepo implements format.Browsable.
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	if c.Repo.Kind == repo.Group {
		return format.GroupBrowse(h, c)
	}
	var recs []chartRecord
	if c.Repo.Kind == repo.Proxy {
		recs = h.upstreamRecords(c)
	} else {
		keys, err := c.Meta.List(h.ns(c))
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			var rec chartRecord
			if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); ok {
				recs = append(recs, rec)
			}
		}
	}
	byName := map[string][]string{}
	byNameTime := map[string]time.Time{}
	for _, rec := range recs {
		byName[rec.Name] = append(byName[rec.Name], rec.Version)
		if rec.UploadedAt.After(byNameTime[rec.Name]) {
			byNameTime[rec.Name] = rec.UploadedAt
		}
	}
	entries := make([]format.BrowseEntry, 0, len(byName))
	for name, versions := range byName {
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		entries = append(entries, format.BrowseEntry{Name: name, Versions: versions, UpdatedAt: byNameTime[name]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// Inspect implements format.Inspectable for the component detail page.
func (h *Handler) Inspect(c *format.Context, baseURL, name string) (format.ComponentDetail, bool) {
	var allRecs []chartRecord
	switch c.Repo.Kind {
	case repo.Group:
		allRecs = h.groupRecords(c)
	case repo.Proxy:
		allRecs = h.upstreamRecords(c)
	default:
		allRecs = h.records(c)
	}
	var matching []chartRecord
	for _, rec := range allRecs {
		if rec.Name == name {
			matching = append(matching, rec)
		}
	}
	if len(matching) == 0 {
		return format.ComponentDetail{}, false
	}
	sort.Slice(matching, func(i, j int) bool { return matching[i].Version > matching[j].Version })

	versions := make([]format.VersionInfo, len(matching))
	for i, rec := range matching {
		versions[i] = format.VersionInfo{
			Version:     rec.Version,
			DownloadURL: fmt.Sprintf("%s/repository/%s/%s", baseURL, c.Repo.Name, rec.Filename),
			PublishedAt: rec.UploadedAt,
		}
	}

	snippet := fmt.Sprintf("helm repo add forge %s/repository/%s\nhelm install %s forge/%s",
		baseURL, c.Repo.Name, name, name)
	return format.ComponentDetail{
		Name:           name,
		Versions:       versions,
		Description:    matching[0].Description,
		InstallSnippet: snippet,
	}, true
}
