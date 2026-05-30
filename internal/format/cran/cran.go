// Package cran implements a CRAN-style R package repository (source packages).
//
// CRAN is the simplest of the four: static files in a fixed layout plus one
// aggregate index file (PACKAGES) in Debian-control format.
//
//	PUT /src/contrib/{pkg}_{ver}.tar.gz  -> publish (DESCRIPTION parsed for index)
//	GET /src/contrib/PACKAGES            -> generated control-format index
//	GET /src/contrib/PACKAGES.gz         -> gzipped index
//	GET /src/contrib/{pkg}_{ver}.tar.gz  -> download
//
// Group: read-only fan-out. PACKAGES merges all members (dedup by Package+Version,
// first member wins); downloads try each member in order.
//
// TODO: PACKAGES.rds (R-serialized index; needs an rds writer or an R process)
// and per-OS binary package trees under /bin/. Modern R reads PACKAGES fine for
// source installs, but renv/pak prefer .rds.
package cran

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"forge/internal/format"
	"forge/internal/repo"
)

type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "cran" }

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + ":cran" }

type pkgRecord struct {
	Package string `json:"package"`
	Version string `json:"version"`
	Depends string `json:"depends,omitempty"`
	Imports string `json:"imports,omitempty"`
	License string `json:"license,omitempty"`
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch {
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES":
		w.Header().Set("Content-Type", "text/plain")
		w.Write(h.packages(c))
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES.gz":
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		gz.Write(h.packages(c))
		gz.Close()
	case r.Method == http.MethodPut && strings.HasPrefix(c.Sub, "src/contrib/") && strings.HasSuffix(c.Sub, ".tar.gz"):
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.publish(w, r, c)
	case r.Method == http.MethodGet && strings.HasSuffix(c.Sub, ".tar.gz"):
		h.download(w, c)
	default:
		if c.Repo.Kind == repo.Proxy && r.Method == http.MethodGet {
			h.proxy(w, c)
			return
		}
		http.Error(w, "unsupported cran request", http.StatusNotFound)
	}
}

func (h *Handler) publish(w http.ResponseWriter, r *http.Request, c *format.Context) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := parseDescription(body)
	if err != nil {
		http.Error(w, "invalid package: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := c.Blob.Put(c.Key(c.Sub), bytes.NewReader(body)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.Meta.PutJSON(h.ns(c), rec.Package+"_"+rec.Version, rec)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored %s %s\n", rec.Package, rec.Version)
}

func (h *Handler) download(w http.ResponseWriter, c *format.Context) {
	if c.Repo.Kind == repo.Group {
		h.groupDownload(w, c)
		return
	}
	rc, err := c.Blob.Get(c.Key(c.Sub))
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, rc)
}

func (h *Handler) groupDownload(w http.ResponseWriter, c *format.Context) {
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		if rc, err := mc.Blob.Get(mc.Key(c.Sub)); err == nil {
			defer rc.Close()
			w.Header().Set("Content-Type", "application/gzip")
			io.Copy(w, rc)
			return
		}
		// For proxy members, attempt upstream fetch and cache.
		if mc.Repo.Kind == repo.Proxy {
			url := strings.TrimRight(mc.Repo.Upstream, "/") + "/" + c.Sub
			resp, err := mc.HTTP.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				var buf bytes.Buffer
				tee := io.TeeReader(resp.Body, &buf)
				w.Header().Set("Content-Type", "application/gzip")
				io.Copy(w, tee)
				mc.Blob.Put(mc.Key(c.Sub), &buf)
				return
			}
		}
	}
	http.NotFound(w, nil)
}

func (h *Handler) proxy(w http.ResponseWriter, c *format.Context) {
	url := strings.TrimRight(c.Repo.Upstream, "/") + "/" + c.Sub
	resp, err := c.HTTP.Get(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	io.Copy(io.MultiWriter(w, &buf), resp.Body)
	c.Blob.Put(c.Key(c.Sub), &buf)
}

// packages returns the PACKAGES index for this repo (or merged across members
// for a group).
func (h *Handler) packages(c *format.Context) []byte {
	var recs []pkgRecord
	if c.Repo.Kind == repo.Group {
		recs = h.groupPkgRecords(c)
	} else {
		recs = h.pkgRecords(c)
	}
	return buildPackages(recs)
}

// pkgRecords loads all package records from this repo's meta namespace.
func (h *Handler) pkgRecords(c *format.Context) []pkgRecord {
	keys, _ := c.Meta.List(h.ns(c))
	sort.Strings(keys)
	var recs []pkgRecord
	for _, k := range keys {
		var rec pkgRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); ok {
			recs = append(recs, rec)
		}
	}
	return recs
}

// groupPkgRecords merges package records from all members, deduplicating by
// Package_Version key (first member with a given package+version wins).
func (h *Handler) groupPkgRecords(c *format.Context) []pkgRecord {
	seen := map[string]bool{}
	var all []pkgRecord
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		for _, rec := range h.pkgRecords(mc) {
			key := rec.Package + "_" + rec.Version
			if !seen[key] {
				seen[key] = true
				all = append(all, rec)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Package < all[j].Package })
	return all
}

// buildPackages is the pure generator for the PACKAGES index so tests can
// call it without a live meta store.
func buildPackages(recs []pkgRecord) []byte {
	var b strings.Builder
	for _, rec := range recs {
		fmt.Fprintf(&b, "Package: %s\nVersion: %s\n", rec.Package, rec.Version)
		if rec.Depends != "" {
			fmt.Fprintf(&b, "Depends: %s\n", rec.Depends)
		}
		if rec.Imports != "" {
			fmt.Fprintf(&b, "Imports: %s\n", rec.Imports)
		}
		if rec.License != "" {
			fmt.Fprintf(&b, "License: %s\n", rec.License)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// parseDescription extracts control fields from {pkg}/DESCRIPTION inside the tarball.
func parseDescription(tgz []byte) (pkgRecord, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return pkgRecord{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return pkgRecord{}, err
		}
		if path.Base(hdr.Name) == "DESCRIPTION" {
			data, _ := io.ReadAll(tr)
			return scanDescription(data), nil
		}
	}
	return pkgRecord{}, fmt.Errorf("DESCRIPTION not found")
}

// scanDescription parses control format, joining continuation lines (leading space).
func scanDescription(data []byte) pkgRecord {
	fields := map[string]string{}
	var curKey string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && curKey != "" {
			fields[curKey] += " " + strings.TrimSpace(line)
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		curKey = strings.TrimSpace(key)
		fields[curKey] = strings.TrimSpace(val)
	}
	return pkgRecord{
		Package: fields["Package"], Version: fields["Version"],
		Depends: fields["Depends"], Imports: fields["Imports"],
		License: fields["License"],
	}
}
