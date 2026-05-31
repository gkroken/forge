// Package cran implements a CRAN-style R package repository (source packages).
//
// CRAN is the simplest of the four: static files in a fixed layout plus one
// aggregate index file (PACKAGES) in Debian-control format.
//
//	PUT /src/contrib/{pkg}_{ver}.tar.gz  -> publish (DESCRIPTION parsed for index)
//	GET /src/contrib/PACKAGES            -> generated control-format index
//	GET /src/contrib/PACKAGES.gz         -> gzipped index
//	GET /src/contrib/PACKAGES.rds        -> R-serialized index (preferred by renv/pak)
//	GET /src/contrib/{pkg}_{ver}.tar.gz  -> download
//
// Group: read-only fan-out. All index formats merge across members.
//
// PACKAGES.rds is a gzip-compressed R serialization (XDR v2) of a character
// matrix: rows are packages, columns are Package/Version/Depends/Imports/License.
// Missing field values are stored as NA_character_. This matches CRAN's own
// PACKAGES.rds format and is consumable by renv and pak without needing R.
package cran

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"

	"forge/internal/format"
	"forge/internal/proxy"
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
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES.rds":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buildPackagesRDS(h.allPkgRecords(c)))
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
		// Proxy repos fetch from upstream on a cache miss.
		if c.Repo.Kind == repo.Proxy {
			h.proxy(w, c)
			return
		}
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
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/" + c.Sub
	key := c.Key(c.Sub)
	cfg := proxy.Config{TTL: c.Repo.ProxyTTL, Auth: c.Repo.ProxyAuth}
	if c.Metrics != nil {
		m, repo := c.Metrics, c.Repo.Name
		cfg.RecordHit = func() { m.CacheHits.WithLabelValues(repo).Inc() }
		cfg.RecordMiss = func() { m.CacheMisses.WithLabelValues(repo).Inc() }
	}
	f := proxy.New(c.HTTP, cfg)
	rc, ct, err := f.Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if errors.Is(err, proxy.ErrNotFound) {
		http.NotFound(w, nil)
		return
	}
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer rc.Close()
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	io.Copy(w, rc)
}

// allPkgRecords returns merged records for group repos, or direct records for
// hosted/proxy repos.
func (h *Handler) allPkgRecords(c *format.Context) []pkgRecord {
	if c.Repo.Kind == repo.Group {
		return h.groupPkgRecords(c)
	}
	return h.pkgRecords(c)
}

// packages returns the PACKAGES index for this repo.
func (h *Handler) packages(c *format.Context) []byte {
	return buildPackages(h.allPkgRecords(c))
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

// --- PACKAGES.rds: R XDR serialization ------------------------------------
//
// R's binary serialization format (version 2, XDR / big-endian) for a
// character matrix. The structure mirrors what CRAN publishes so renv and pak
// can consume it directly.
//
// Binary layout (after gzip decompression):
//
//	"X\n"              format marker (XDR)
//	int32 version=2
//	int32 R-written-version  (3.6.3 = 0x00030603)
//	int32 R-min-version      (2.3.0 = 0x00020300)
//	STRSXP|HAS_ATTR           matrix elements, column-major
//	  int32 nrows*ncols
//	  CHARSXPs...             NA_character_ for absent optional fields
//	LISTSXP|HAS_TAG           attribute 1: dim
//	  SYMSXP "dim"
//	  INTSXP [nrows, ncols]
//	  LISTSXP|HAS_TAG         attribute 2: dimnames
//	    SYMSXP "dimnames"
//	    VECSXP [NULL, col-name STRSXP]
//	    NILVALUE_SXP           end of pairlist

func buildPackagesRDS(recs []pkgRecord) []byte {
	cols := []string{"Package", "Version", "Depends", "Imports", "License"}
	nrows, ncols := len(recs), len(cols)

	var w rdsWriter
	w.raw([]byte("X\n"))
	w.i32(2)           // serialization version 2
	w.i32(0x00030603)  // written by R 3.6.3
	w.i32(0x00020300)  // readable by R >= 2.3.0

	// STRSXP with HAS_ATTR: column-major matrix data.
	const (
		rSTRSXP  = 16
		rCHARSXP = 9
		rINTSXP  = 13
		rVECSXP  = 19
		rSYMSXP  = 1
		rHasAttr = 1 << 9  // bit 9: HAS_ATTR
		rHasTag  = 1 << 10 // bit 10: HAS_TAG
		rNIL     = 254     // NILVALUE_SXP
	)

	w.i32(rSTRSXP | rHasAttr)
	w.i32(int32(nrows * ncols))
	for c := 0; c < ncols; c++ {
		for r := 0; r < nrows; r++ {
			w.charsxp(pkgField(recs[r], cols[c]))
		}
	}

	// Attribute 1: dim = c(nrows, ncols)
	w.i32(rLISTPLY | rHasTag) // LISTSXP|HAS_TAG node
	w.sym("dim")
	w.i32(rINTSXP)
	w.i32(2)
	w.i32(int32(nrows))
	w.i32(int32(ncols))
	// CDR: attribute 2 (dimnames node)
	w.i32(rLISTPLY | rHasTag) // LISTSXP|HAS_TAG node
	w.sym("dimnames")
	// CAR: list(NULL, col_names)
	w.i32(rVECSXP)
	w.i32(2) // 2 elements: row names (NULL) + col names
	w.i32(rNIL)
	w.i32(rSTRSXP)
	w.i32(int32(ncols))
	for _, col := range cols {
		w.charsxpRaw(col) // column names are never empty
	}
	// CDR: end of pairlist
	w.i32(rNIL)

	// Gzip-compress the serialized bytes.
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	gz.Write(w.buf.Bytes())
	gz.Close()
	return out.Bytes()
}

const rLISTPLY = 2 // LISTSXP

func pkgField(rec pkgRecord, col string) string {
	switch col {
	case "Package":
		return rec.Package
	case "Version":
		return rec.Version
	case "Depends":
		return rec.Depends
	case "Imports":
		return rec.Imports
	case "License":
		return rec.License
	}
	return ""
}

// rdsWriter writes R's XDR serialization byte stream.
type rdsWriter struct{ buf bytes.Buffer }

func (w *rdsWriter) raw(b []byte) { w.buf.Write(b) }

// i32 writes a big-endian int32.
func (w *rdsWriter) i32(v int32) {
	w.buf.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// charsxpRaw writes a CHARSXP with the given (non-empty) string.
func (w *rdsWriter) charsxpRaw(s string) {
	w.i32(9) // CHARSXP, CE_NATIVE encoding
	w.i32(int32(len(s)))
	w.buf.WriteString(s)
}

// charsxp writes a CHARSXP, using NA_character_ (length=-1) for empty strings.
// This matches CRAN's convention: absent optional fields are NA, not "".
func (w *rdsWriter) charsxp(s string) {
	if s == "" {
		w.i32(9)  // CHARSXP
		w.i32(-1) // length -1 = NA_character_
		return
	}
	w.charsxpRaw(s)
}

// sym writes a SYMSXP followed by a CHARSXP for the symbol name.
func (w *rdsWriter) sym(name string) {
	w.i32(1) // SYMSXP
	w.charsxpRaw(name)
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

// BrowseRepo implements format.Browsable. allPkgRecords already handles groups.
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	recs := h.allPkgRecords(c)
	byName := map[string][]string{}
	for _, r := range recs {
		byName[r.Package] = append(byName[r.Package], r.Version)
	}
	entries := make([]format.BrowseEntry, 0, len(byName))
	for name, versions := range byName {
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		entries = append(entries, format.BrowseEntry{Name: name, Versions: versions})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
