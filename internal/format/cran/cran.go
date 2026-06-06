// Package cran implements a CRAN-style R package repository.
//
// Source packages (all platforms):
//
//	PUT /src/contrib/{pkg}_{ver}.tar.gz  -> publish (DESCRIPTION parsed for index)
//	GET /src/contrib/PACKAGES            -> generated control-format index
//	GET /src/contrib/PACKAGES.gz         -> gzipped index
//	GET /src/contrib/PACKAGES.rds        -> R-serialized index (preferred by renv/pak)
//	GET /src/contrib/{pkg}_{ver}.tar.gz  -> download
//
// Binary packages (Windows .zip, macOS .tgz):
//
//	PUT /bin/{platform}/contrib/{rver}/{pkg}_{ver}.{zip|tgz}  -> publish
//	GET /bin/{platform}/contrib/{rver}/PACKAGES[.gz|.rds]     -> index
//	GET /bin/{platform}/contrib/{rver}/{pkg}_{ver}.{zip|tgz}  -> download
//
// Platform examples: "windows", "macosx/x86_64", "macosx/big-sur-arm64".
// Group: read-only fan-out for source and binary packages. All index formats merge.
//
// PACKAGES.rds is a gzip-compressed R serialization (XDR v2) of a character
// matrix: rows are packages, columns are Package/Version/Depends/Imports/License.
// Missing field values are stored as NA_character_. This matches CRAN's own
// PACKAGES.rds format and is consumable by renv and pak without needing R.
package cran

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
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
func (h *Handler) Format() string { return "cran" }

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + "+cran" }

type pkgRecord struct {
	Package          string    `json:"package"`
	Version          string    `json:"version"`
	Depends          string    `json:"depends,omitempty"`
	Imports          string    `json:"imports,omitempty"`
	License          string    `json:"license,omitempty"`
	NeedsCompilation string    `json:"needsCompilation,omitempty"`
	Built            string    `json:"built,omitempty"`
	Archs            string    `json:"archs,omitempty"`
	OStype           string    `json:"ostype,omitempty"`
	UploadedAt       time.Time `json:"uploadedAt,omitempty"`
}

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	switch {
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES":
		if c.Repo.Kind == repo.Proxy {
			h.proxy(w, c)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(h.packages(c))
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES.gz":
		if c.Repo.Kind == repo.Proxy {
			h.proxy(w, c)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		gz.Write(h.packages(c))
		gz.Close()
	case r.Method == http.MethodGet && c.Sub == "src/contrib/PACKAGES.rds":
		if c.Repo.Kind == repo.Proxy {
			h.proxy(w, c)
			return
		}
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
	case r.Method == http.MethodDelete && strings.HasPrefix(c.Sub, "src/contrib/") && strings.HasSuffix(c.Sub, ".tar.gz"):
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot delete from non-hosted repository", http.StatusMethodNotAllowed)
			return
		}
		h.deletePkg(w, c)
	case strings.HasPrefix(c.Sub, "bin/"):
		h.serveBinary(w, r, c)
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
	rec.UploadedAt = time.Now().UTC()
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
// For proxy members the upstream PACKAGES file is fetched and parsed so the
// group index reflects the full upstream catalogue, not just locally cached tarballs.
func (h *Handler) groupPkgRecords(c *format.Context) []pkgRecord {
	seen := map[string]bool{}
	var all []pkgRecord
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		var recs []pkgRecord
		if mc.Repo.Kind == repo.Proxy {
			recs = h.upstreamPkgRecords(mc)
		} else {
			recs = h.pkgRecords(mc)
		}
		for _, rec := range recs {
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

// upstreamPkgRecords fetches the upstream PACKAGES file for a proxy member,
// caches it via the proxy store, and parses it into pkgRecord slices.
// Returns nil on any fetch or parse error (group index degrades gracefully).
func (h *Handler) upstreamPkgRecords(mc *format.Context) []pkgRecord {
	key := mc.Key("src/contrib/PACKAGES")
	upURL := strings.TrimRight(mc.Repo.Upstream, "/") + "/src/contrib/PACKAGES"
	cfg := proxy.Config{TTL: mc.Repo.ProxyTTL, Auth: mc.Repo.ProxyAuth}
	f := proxy.New(mc.HTTP, cfg)
	rc, _, err := f.Fetch(key, mc.Repo.Name+":proxy", upURL, mc.Blob, mc.Meta)
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return parsePackagesFile(data)
}

// parsePackagesFile parses a CRAN PACKAGES file (DCF format) into pkgRecord
// slices. Records are separated by blank lines; continuation lines begin with
// whitespace.
func parsePackagesFile(data []byte) []pkgRecord {
	var recs []pkgRecord
	fields := map[string]string{}
	var curKey string
	flush := func() {
		if fields["Package"] != "" {
			recs = append(recs, pkgRecord{
				Package:          fields["Package"],
				Version:          fields["Version"],
				Depends:          fields["Depends"],
				Imports:          fields["Imports"],
				License:          fields["License"],
				NeedsCompilation: fields["NeedsCompilation"],
			})
		}
		fields = map[string]string{}
		curKey = ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && curKey != "" {
			fields[curKey] += " " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		curKey = strings.TrimSpace(k)
		fields[curKey] = strings.TrimSpace(v)
	}
	flush()
	return recs
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
		if rec.NeedsCompilation != "" {
			fmt.Fprintf(&b, "NeedsCompilation: %s\n", rec.NeedsCompilation)
		}
		if rec.Built != "" {
			fmt.Fprintf(&b, "Built: %s\n", rec.Built)
		}
		if rec.Archs != "" {
			fmt.Fprintf(&b, "Archs: %s\n", rec.Archs)
		}
		if rec.OStype != "" {
			fmt.Fprintf(&b, "OS_type: %s\n", rec.OStype)
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
	cols := []string{"Package", "Version", "Depends", "Imports", "License", "NeedsCompilation", "Built", "Archs", "OS_type"}
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
	w.i32(int32(nrows * ncols)) // #nosec G115 -- package counts never approach int32 overflow
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
	w.i32(int32(nrows)) // #nosec G115
	w.i32(int32(ncols)) // #nosec G115
	// CDR: attribute 2 (dimnames node)
	w.i32(rLISTPLY | rHasTag) // LISTSXP|HAS_TAG node
	w.sym("dimnames")
	// CAR: list(NULL, col_names)
	w.i32(rVECSXP)
	w.i32(2) // 2 elements: row names (NULL) + col names
	w.i32(rNIL)
	w.i32(rSTRSXP)
	w.i32(int32(ncols)) // #nosec G115
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
	case "NeedsCompilation":
		return rec.NeedsCompilation
	case "Built":
		return rec.Built
	case "Archs":
		return rec.Archs
	case "OS_type":
		return rec.OStype
	}
	return ""
}

// rdsWriter writes R's XDR serialization byte stream.
type rdsWriter struct{ buf bytes.Buffer }

func (w *rdsWriter) raw(b []byte) { w.buf.Write(b) }

// i32 writes a big-endian int32.
func (w *rdsWriter) i32(v int32) {
	w.buf.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}) // #nosec G115 -- big-endian int32 serialisation
}

// charsxpRaw writes a CHARSXP with the given (non-empty) string.
func (w *rdsWriter) charsxpRaw(s string) {
	w.i32(9) // CHARSXP, CE_NATIVE encoding
	w.i32(int32(len(s))) // #nosec G115 -- string length bounded by CRAN field sizes
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
		License:          fields["License"],
		NeedsCompilation: fields["NeedsCompilation"],
		Built:            fields["Built"],
		Archs:            fields["Archs"],
		OStype:           fields["OS_type"],
	}
}

// --- delete ----------------------------------------------------------------

// deletePkg removes a source package blob and its meta record so it no longer
// appears in the generated PACKAGES index.
func (h *Handler) deletePkg(w http.ResponseWriter, c *format.Context) {
	key := c.Key(c.Sub)
	if _, exists, _ := c.Blob.Stat(key); !exists {
		http.NotFound(w, nil)
		return
	}
	if err := c.Blob.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Meta key mirrors what publish() stores: "{Package}_{Version}"
	metaKey := strings.TrimSuffix(path.Base(c.Sub), ".tar.gz")
	c.Meta.Delete(h.ns(c), metaKey) //nolint:errcheck
	w.WriteHeader(http.StatusNoContent)
}

// --- Binary tree support ---------------------------------------------------

// serveBinary dispatches requests under /bin/.
func (h *Handler) serveBinary(w http.ResponseWriter, r *http.Request, c *format.Context) {
	// Platform enumeration: GET bin/{platform}/contrib/ — no rver or file segment.
	if r.Method == http.MethodGet {
		if rest, ok := strings.CutPrefix(c.Sub, "bin/"); ok {
			if idx := strings.Index(rest, "/contrib/"); idx >= 0 && rest[idx+len("/contrib/"):] == "" {
				h.servePlatformVersions(w, c, rest[:idx])
				return
			}
		}
	}

	platform, rver, file, ok := parseBinPath(c.Sub)
	if !ok {
		http.Error(w, "invalid binary path", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet && (file == "PACKAGES" || file == "PACKAGES.gz" || file == "PACKAGES.rds"):
		h.serveBinIndex(w, c, platform, rver, file)
	case r.Method == http.MethodPut && (strings.HasSuffix(file, ".zip") || strings.HasSuffix(file, ".tgz")):
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot publish to non-hosted repo", http.StatusMethodNotAllowed)
			return
		}
		h.publishBin(w, r, c, platform, rver, file)
	case r.Method == http.MethodGet && (strings.HasSuffix(file, ".zip") || strings.HasSuffix(file, ".tgz")):
		h.downloadBin(w, c)
	case r.Method == http.MethodDelete && (strings.HasSuffix(file, ".zip") || strings.HasSuffix(file, ".tgz")):
		if c.Repo.Kind != repo.Hosted {
			http.Error(w, "cannot delete from non-hosted repository", http.StatusMethodNotAllowed)
			return
		}
		h.deleteBin(w, c, platform, rver, file)
	default:
		http.Error(w, "unsupported binary request", http.StatusNotFound)
	}
}

// serveBinIndex generates and serves the PACKAGES index for one platform+rver.
func (h *Handler) serveBinIndex(w http.ResponseWriter, c *format.Context, platform, rver, file string) {
	if c.Repo.Kind == repo.Proxy {
		h.proxy(w, c)
		return
	}
	var recs []pkgRecord
	if c.Repo.Kind == repo.Group {
		recs = h.groupBinPkgRecords(c, platform, rver)
	} else {
		recs = h.binPkgRecords(c, platform, rver)
	}
	switch file {
	case "PACKAGES":
		w.Header().Set("Content-Type", "text/plain")
		w.Write(buildPackages(recs))
	case "PACKAGES.gz":
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		gz.Write(buildPackages(recs))
		gz.Close()
	case "PACKAGES.rds":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buildPackagesRDS(recs))
	}
}

// publishBin stores a binary package and records its metadata.
func (h *Handler) publishBin(w http.ResponseWriter, r *http.Request, c *format.Context, platform, rver, file string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := parseBinDescription(body, file)
	if err != nil {
		// Fall back to filename when DESCRIPTION is absent or unparseable.
		pkg, ver, ok := parsePkgFilename(file)
		if !ok {
			http.Error(w, "cannot determine package name from: "+file, http.StatusBadRequest)
			return
		}
		rec = pkgRecord{Package: pkg, Version: ver}
	}
	if _, err := c.Blob.Put(c.Key(c.Sub), bytes.NewReader(body)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.Meta.PutJSON(h.binNS(c, platform, rver), rec.Package+"_"+rec.Version, rec)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored binary %s %s (%s/%s)\n", rec.Package, rec.Version, platform, rver)
}

// downloadBin serves a stored binary package, proxying on cache-miss for proxy
// repos and fanning out across members for group repos.
func (h *Handler) downloadBin(w http.ResponseWriter, c *format.Context) {
	if c.Repo.Kind == repo.Group {
		h.groupDownloadBin(w, c)
		return
	}
	rc, err := c.Blob.Get(c.Key(c.Sub))
	if err != nil {
		if c.Repo.Kind == repo.Proxy {
			h.proxy(w, c)
			return
		}
		http.NotFound(w, nil)
		return
	}
	defer rc.Close()
	if strings.HasSuffix(c.Sub, ".zip") {
		w.Header().Set("Content-Type", "application/zip")
	} else {
		w.Header().Set("Content-Type", "application/gzip")
	}
	io.Copy(w, rc)
}

// groupDownloadBin fans out a binary download across group members, trying
// proxy members' upstreams on blob-cache miss (mirrors groupDownload).
func (h *Handler) groupDownloadBin(w http.ResponseWriter, c *format.Context) {
	ct := "application/gzip"
	if strings.HasSuffix(c.Sub, ".zip") {
		ct = "application/zip"
	}
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		if rc, err := mc.Blob.Get(mc.Key(c.Sub)); err == nil {
			defer rc.Close()
			w.Header().Set("Content-Type", ct)
			io.Copy(w, rc)
			return
		}
		if mc.Repo.Kind == repo.Proxy {
			url := strings.TrimRight(mc.Repo.Upstream, "/") + "/" + c.Sub
			resp, err := mc.HTTP.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				var buf bytes.Buffer
				tee := io.TeeReader(resp.Body, &buf)
				w.Header().Set("Content-Type", ct)
				io.Copy(w, tee)
				mc.Blob.Put(mc.Key(c.Sub), &buf) //nolint:errcheck
				return
			}
		}
	}
	http.NotFound(w, nil)
}

// groupBinPkgRecords merges binary package records from all group members for
// a given platform+rver, deduplicating by Package_Version (first member wins).
// For proxy members the upstream binary PACKAGES file is fetched and parsed.
func (h *Handler) groupBinPkgRecords(c *format.Context, platform, rver string) []pkgRecord {
	seen := map[string]bool{}
	var all []pkgRecord
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		var recs []pkgRecord
		if mc.Repo.Kind == repo.Proxy {
			recs = h.upstreamBinPkgRecords(mc, platform, rver)
		} else {
			recs = h.binPkgRecords(mc, platform, rver)
		}
		for _, rec := range recs {
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

// upstreamBinPkgRecords fetches the upstream binary PACKAGES file for a proxy
// member at the given platform+rver, caches it, and parses it into pkgRecord slices.
func (h *Handler) upstreamBinPkgRecords(mc *format.Context, platform, rver string) []pkgRecord {
	sub := "bin/" + platform + "/contrib/" + rver + "/PACKAGES"
	key := mc.Key(sub)
	upURL := strings.TrimRight(mc.Repo.Upstream, "/") + "/" + sub
	cfg := proxy.Config{TTL: mc.Repo.ProxyTTL, Auth: mc.Repo.ProxyAuth}
	f := proxy.New(mc.HTTP, cfg)
	rc, _, err := f.Fetch(key, mc.Repo.Name+":proxy", upURL, mc.Blob, mc.Meta)
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return parsePackagesFile(data)
}

// deleteBin removes a binary package blob and its meta record.
func (h *Handler) deleteBin(w http.ResponseWriter, c *format.Context, platform, rver, file string) {
	key := c.Key(c.Sub)
	if _, exists, _ := c.Blob.Stat(key); !exists {
		http.NotFound(w, nil)
		return
	}
	if err := c.Blob.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pkg, ver, ok := parsePkgFilename(file); ok {
		c.Meta.Delete(h.binNS(c, platform, rver), pkg+"_"+ver) //nolint:errcheck
	}
	w.WriteHeader(http.StatusNoContent)
}

// servePlatformVersions lists the R versions that have at least one binary
// package published for the given platform. Returns a newline-delimited plain
// text list; always 200 (empty body when nothing is published).
func (h *Handler) servePlatformVersions(w http.ResponseWriter, c *format.Context, platform string) {
	prefix := c.Repo.Name + "/bin/" + platform + "/contrib/"
	keys, _ := c.Blob.List(prefix)
	seen := map[string]bool{}
	for _, key := range keys {
		rel := strings.TrimPrefix(key, prefix)
		if rver, _, ok := strings.Cut(rel, "/"); ok && rver != "" {
			seen[rver] = true
		}
	}
	versions := make([]string, 0, len(seen))
	for v := range seen {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	w.Header().Set("Content-Type", "text/plain")
	if len(versions) > 0 {
		w.Write([]byte(strings.Join(versions, "\n") + "\n")) //nolint:errcheck
	}
}

// binNS returns the meta namespace for binary packages of a given platform+rver.
// Uses + as separator and replaces any / in multi-segment platform paths so the
// namespace maps to a single flat directory on every OS (colons are illegal in
// Windows directory names; slashes would create unexpected subdirectories).
func (h *Handler) binNS(c *format.Context, platform, rver string) string {
	safePlatform := strings.ReplaceAll(platform, "/", "+")
	return c.Repo.Name + "+cran+bin+" + safePlatform + "+" + rver
}

// binPkgRecords loads all binary package records for a given platform+rver.
func (h *Handler) binPkgRecords(c *format.Context, platform, rver string) []pkgRecord {
	keys, _ := c.Meta.List(h.binNS(c, platform, rver))
	sort.Strings(keys)
	var recs []pkgRecord
	for _, k := range keys {
		var rec pkgRecord
		if ok, _ := c.Meta.GetJSON(h.binNS(c, platform, rver), k, &rec); ok {
			recs = append(recs, rec)
		}
	}
	return recs
}

// parseBinPath splits a sub-path like "bin/windows/contrib/4.4/PACKAGES" into
// (platform="windows", rver="4.4", file="PACKAGES"). Also handles multi-segment
// platform paths e.g. "bin/macosx/x86_64/contrib/4.4/pkg_1.0.0.tgz".
func parseBinPath(sub string) (platform, rver, file string, ok bool) {
	rest, found := strings.CutPrefix(sub, "bin/")
	if !found {
		return "", "", "", false
	}
	platform, after, found := strings.Cut(rest, "/contrib/")
	if !found {
		return "", "", "", false
	}
	rver, file, found = strings.Cut(after, "/")
	return platform, rver, file, found && file != ""
}

// parsePkgFilename extracts Package and Version from a filename like
// "mypackage_1.0.0.zip" or "mypackage_1.0.0.tgz".
func parsePkgFilename(filename string) (pkg, ver string, ok bool) {
	name := strings.TrimSuffix(strings.TrimSuffix(filename, ".zip"), ".tgz")
	idx := strings.LastIndex(name, "_")
	if idx < 0 {
		return "", "", false
	}
	return name[:idx], name[idx+1:], true
}

// parseBinDescription reads the DESCRIPTION file from a binary package archive.
// Windows packages are .zip; macOS/Linux binary packages are .tgz.
func parseBinDescription(data []byte, filename string) (pkgRecord, error) {
	if strings.HasSuffix(filename, ".zip") {
		return parseDescriptionFromZip(data)
	}
	return parseDescription(data)
}

// parseDescriptionFromZip reads {pkg}/DESCRIPTION from a Windows .zip binary.
func parseDescriptionFromZip(data []byte) (pkgRecord, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return pkgRecord{}, err
	}
	for _, f := range zr.File {
		if path.Base(f.Name) == "DESCRIPTION" {
			rc, err := f.Open()
			if err != nil {
				return pkgRecord{}, err
			}
			defer rc.Close()
			content, err := io.ReadAll(rc)
			if err != nil {
				return pkgRecord{}, err
			}
			return scanDescription(content), nil
		}
	}
	return pkgRecord{}, fmt.Errorf("DESCRIPTION not found in zip")
}

// BrowseRepo implements format.Browsable. Source and binary packages are merged
// by package name; versions are deduplicated so a package published for multiple
// platforms appears once per version.
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	// Source packages (group-aware via allPkgRecords).
	byName := map[string]map[string]bool{}
	for _, r := range h.allPkgRecords(c) {
		if byName[r.Package] == nil {
			byName[r.Package] = map[string]bool{}
		}
		byName[r.Package][r.Version] = true
	}

	// Binary packages — scan blobs under bin/. For group repos, scan each member.
	scanBinBlobs := func(repoName string) {
		prefix := repoName + "/bin/"
		keys, _ := c.Blob.List(prefix)
		for _, key := range keys {
			sub := strings.TrimPrefix(key, repoName+"/")
			_, _, file, ok := parseBinPath(sub)
			if !ok {
				continue
			}
			pkg, ver, ok := parsePkgFilename(file)
			if !ok {
				continue
			}
			if byName[pkg] == nil {
				byName[pkg] = map[string]bool{}
			}
			byName[pkg][ver] = true
		}
	}
	if c.Repo.Kind == repo.Group {
		for _, name := range c.Repo.Members {
			if mc, ok := c.MemberCtx(name); ok {
				scanBinBlobs(mc.Repo.Name)
			}
		}
	} else {
		scanBinBlobs(c.Repo.Name)
	}

	entries := make([]format.BrowseEntry, 0, len(byName))
	for name, versionSet := range byName {
		versions := make([]string, 0, len(versionSet))
		for v := range versionSet {
			versions = append(versions, v)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		entries = append(entries, format.BrowseEntry{Name: name, Versions: versions})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
