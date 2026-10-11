package cran

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5" // #nosec G501 -- CRAN's index names files by MD5; not a security use
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"forge/internal/format"
	"forge/internal/proxy"
)

// Pinned proxy tarballs (forge-issues/008).
//
// A CRAN proxy keeps the first bytes it cached for a package version: CRAN
// never republishes a version, so a cached src/contrib/{pkg}_{ver}.tar.gz is
// served from the store without asking upstream again, whatever contentMaxAge
// says (download in cran.go never consults the fetcher on a hit). The index is
// different — it gains entries — so it is refreshed per metadataMaxAge and
// passed through as upstream wrote it.
//
// When upstream does republish a version (a resynced mirror, a misbehaving
// internal CRAN-like repository), the refreshed index carries an MD5sum the
// pinned file does not have, and a client that checks it (renv can; Skald
// does) gets a mismatch it cannot explain. So the proxy's index always
// describes the file forge serves: when an MD5sum disagrees with a pinned
// tarball, PACKAGES, PACKAGES.gz and PACKAGES.rds are all derived from the
// upstream PACKAGES with that file's MD5sum (and upstream's in
// UpstreamMD5sum), and forge logs that upstream changed a published version. Deleting the cached tarball takes upstream's
// new bytes on the next download.
//
// The pins are one document per proxy — {pkg}_{ver} → MD5 of the cached file
// — because an index request may consult every package, and a read per
// package per index request is not affordable. An entry is only trusted after
// it is checked against the proxy's cache entry, so a tarball deleted by any
// path (delete, retention, trash) needs no hook here: its stale pin is
// ignored, and a re-fetch replaces it.

const pinsKey = "src"

// upstreamMD5Field is added to a corrected index entry, giving the MD5sum
// upstream's index gives for that version.
const upstreamMD5Field = "UpstreamMD5sum"

type pinDoc struct {
	MD5 map[string]string `json:"md5"` // "{pkg}_{ver}" → MD5 of the cached tarball
}

func (h *Handler) pinNS(c *format.Context) string { return c.Repo.Name + "+cran-pins" }

func (h *Handler) loadPins(c *format.Context) pinDoc {
	var d pinDoc
	c.Meta.GetJSON(h.pinNS(c), pinsKey, &d) //nolint:errcheck
	if d.MD5 == nil {
		d.MD5 = map[string]string{}
	}
	return d
}

// cachedMD5 is the MD5 of the tarball the proxy currently has cached under
// sub, or ok false when it has none. Entries cached before the fetcher
// recorded checksums are hashed from the store once; known, when non-empty,
// is the pin already recorded for such an entry, and is trusted instead.
func (h *Handler) cachedMD5(c *format.Context, sub, known string) (string, bool) {
	key := c.Key(sub)
	if _, ok, _ := c.Blob.Stat(key); !ok {
		return "", false
	}
	var e proxy.CacheEntry
	if ok, _ := c.Meta.GetJSON(c.Repo.Name+":proxy", key, &e); ok && e.MD5 != "" {
		return e.MD5, true
	}
	if known != "" {
		return known, true
	}
	rc, err := c.Blob.Get(key)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	hm := md5.New() // #nosec G401
	if _, err := io.Copy(hm, rc); err != nil {
		return "", false
	}
	return hex.EncodeToString(hm.Sum(nil)), true
}

// pinTarball records the MD5 of the tarball a proxy just served for sub, when
// sub is a current-path source tarball and the pin is missing or out of date.
func (h *Handler) pinTarball(c *format.Context, sub string) {
	pkg, ver, ok := nameVersionFromFilename(sub)
	if !ok || sub != "src/contrib/"+pkg+"_"+ver+".tar.gz" {
		return
	}
	pv := pkg + "_" + ver
	d := h.loadPins(c)
	sum, ok := h.cachedMD5(c, sub, d.MD5[pv])
	if !ok || d.MD5[pv] == sum {
		return
	}
	// Read-modify-write: a concurrent pin can be lost. It is restored the
	// next time that tarball is downloaded; until then its index entry is
	// passed through unpatched, which is today's behaviour, not a new risk.
	d.MD5[pv] = sum
	c.Meta.PutJSON(h.pinNS(c), pinsKey, d) //nolint:errcheck
}

// warned remembers which republished versions have been logged, so a
// mismatch is reported once per process rather than per index request.
var warned sync.Map

// serveProxyIndex serves a CRAN proxy's PACKAGES, PACKAGES.gz or PACKAGES.rds:
// upstream's own file, unless the upstream index gives a pinned tarball an
// MD5sum it does not have — then all three are derived from the corrected
// upstream PACKAGES.
func (h *Handler) serveProxyIndex(w http.ResponseWriter, c *format.Context) {
	pins := h.loadPins(c)
	text, ok := h.upstreamIndexText(c)
	// UpstreamMD5sum is forge's statement, so one upstream wrote is dropped;
	// with nothing to correct and nothing to drop, upstream's files pass
	// through byte for byte.
	if !ok || (len(pins.MD5) == 0 && !bytes.Contains(text, []byte(upstreamMD5Field+":"))) {
		h.proxy(w, c)
		return
	}
	fixed, changed := h.correctMD5s(c, text, pins)
	if !changed {
		h.proxy(w, c)
		return
	}
	switch c.Sub {
	case "src/contrib/PACKAGES":
		w.Header().Set("Content-Type", "text/plain")
		w.Write(fixed) //nolint:errcheck
	case "src/contrib/PACKAGES.gz":
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		gz.Write(fixed) //nolint:errcheck
		gz.Close()
	default: // PACKAGES.rds
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(dcfMatrixRDS(fixed)) //nolint:errcheck
	}
}

// upstreamIndexText is the upstream src/contrib/PACKAGES, through the proxy
// cache under metadataMaxAge like the index itself.
func (h *Handler) upstreamIndexText(c *format.Context) ([]byte, bool) {
	key := c.Key("src/contrib/PACKAGES")
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/src/contrib/PACKAGES"
	rc, _, err := proxy.New(c.HTTP, c.ProxyMetadataConfig()).Fetch(key, c.Repo.Name+":proxy", upURL, c.Blob, c.Meta)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return b, err == nil
}

// correctMD5s rewrites each MD5sum in the DCF text that disagrees with the
// tarball the proxy serves for that package version. A pin is used only
// while the proxy still has that tarball cached with that MD5.
func (h *Handler) correctMD5s(c *format.Context, text []byte, pins pinDoc) ([]byte, bool) {
	var out bytes.Buffer
	changed := false
	stanza := []string{}
	flush := func() {
		kept := stanza[:0]
		inUpstreamField := false
		for _, l := range stanza {
			cont := l[0] == ' ' || l[0] == '\t'
			if strings.HasPrefix(l, upstreamMD5Field+":") || (cont && inUpstreamField) {
				inUpstreamField, changed = true, true
				continue
			}
			inUpstreamField = false
			kept = append(kept, l)
		}
		stanza = kept
		pkg, ver, md5Line := "", "", -1
		for i, l := range stanza {
			switch {
			case strings.HasPrefix(l, "Package:"):
				pkg = strings.TrimSpace(strings.TrimPrefix(l, "Package:"))
			case strings.HasPrefix(l, "Version:"):
				ver = strings.TrimSpace(strings.TrimPrefix(l, "Version:"))
			case strings.HasPrefix(l, "MD5sum:"):
				md5Line = i
			}
		}
		pv := pkg + "_" + ver
		if pin, ok := pins.MD5[pv]; ok && md5Line >= 0 {
			upstream := strings.TrimSpace(strings.TrimPrefix(stanza[md5Line], "MD5sum:"))
			if upstream != pin {
				if cur, ok := h.cachedMD5(c, "src/contrib/"+pv+".tar.gz", pin); ok && cur == pin {
					// upstream's claim stays visible, so a client can still
					// refuse a version whose first bytes upstream disowns;
					// R and pak ignore fields they do not know.
					stanza[md5Line] = "MD5sum: " + pin + "\n" + upstreamMD5Field + ": " + upstream
					changed = true
					if _, seen := warned.LoadOrStore(c.Repo.Name+"\x00"+pv+"\x00"+upstream, true); !seen {
						slog.Warn("cran proxy: upstream changed a published version; serving the bytes cached first",
							"repo", c.Repo.Name, "package", pkg, "version", ver,
							"cached_md5", pin, "upstream_md5", upstream,
							"hint", "delete the cached tarball to take upstream's bytes")
					}
				}
			}
		}
		for _, l := range stanza {
			out.WriteString(l)
			out.WriteByte('\n')
		}
		stanza = stanza[:0]
	}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(l) == "" {
			if len(stanza) > 0 {
				flush()
				out.WriteByte('\n')
			}
			continue
		}
		stanza = append(stanza, l)
	}
	if len(stanza) > 0 {
		flush()
	}
	if sc.Err() != nil {
		return nil, false
	}
	return out.Bytes(), changed
}

// dcfMatrixRDS serializes DCF text the way tools::write_PACKAGES writes
// PACKAGES.rds: a character matrix, one row per stanza, one column per field
// (in first-seen order), NA where a stanza lacks the field.
func dcfMatrixRDS(text []byte) []byte {
	var cols []string
	colIdx := map[string]int{}
	var rows []map[string]string
	cur := map[string]string{}
	last := ""
	flush := func() {
		if len(cur) > 0 {
			rows = append(rows, cur)
		}
		cur, last = map[string]string{}, ""
	}
	for _, l := range strings.Split(string(text), "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.TrimSpace(l) == "":
			flush()
		case (l[0] == ' ' || l[0] == '\t') && last != "":
			// Continuation lines join with a newline, as read.dcf does; a
			// value that starts on a continuation line has no leading one.
			if cur[last] != "" {
				cur[last] += "\n"
			}
			cur[last] += strings.TrimSpace(l)
		default:
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			if _, seen := colIdx[k]; !seen {
				colIdx[k] = len(cols)
				cols = append(cols, k)
			}
			cur[k], last = strings.TrimSpace(v), k
		}
	}
	flush()
	return writeRDSMatrix(cols, len(rows), func(r, c int) string { return rows[r][cols[c]] })
}
