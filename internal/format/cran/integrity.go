package cran

import (
	"compress/gzip"
	"io"
	"path"
	"strings"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/repo"
)

// Compile-time assertion that the handler implements the integrity seam.
var _ format.IntegrityChecker = (*Handler)(nil)

// VerifyIntegrity implements format.IntegrityChecker.
//
// CRAN's source of truth is the pkgRecord set — "{repo}+cran" for source
// packages, "{repo}+cran+bin+{platform}+{rver}" per binary tree — from which
// the PACKAGES indexes are generated. Records store no checksum, so the full
// mode's corruption check is the gzip container itself: reading a .tar.gz to
// EOF validates its CRC32, which any byte flip in the stream breaks.
//
//   - package record whose tarball blob is gone       → missing
//   - tarball blob no record claims                   → orphan
//   - full mode: gzip stream fails to decode to EOF   → mismatch
//
// Binary trees are discovered from the blobs present; a platform tree whose
// blobs are all gone cannot be enumerated (meta.Store has no namespace
// listing), so its dangling records go unreported — an accepted gap.
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	if c.Repo.Kind == repo.Proxy {
		return integrity.VerifyProxyCache(c.Repo.Name, c.Blob, c.Meta)
	}
	var res integrity.Result
	if c.Repo.Kind != repo.Hosted {
		return res, nil
	}

	blobs, err := c.Blob.List(c.Repo.Name + "/")
	if err != nil {
		return res, err
	}
	res.BlobsChecked = len(blobs)

	// Index blobs by tree: source packages + one entry per discovered
	// binary (platform, rver) tree. pkgKey = "{Package}_{Version}".
	type tree struct {
		ns    string
		dir   string            // blob key prefix for this tree
		byPkg map[string]string // pkgKey → blob key
	}
	srcTree := &tree{ns: h.ns(c), dir: c.Repo.Name + "/src/contrib/", byPkg: map[string]string{}}
	trees := map[string]*tree{"": srcTree}
	for _, k := range blobs {
		sub := strings.TrimPrefix(k, c.Repo.Name+"/")
		switch {
		case strings.HasPrefix(sub, "src/contrib/") && strings.HasSuffix(sub, ".tar.gz"):
			if path.Dir(sub) != "src/contrib" {
				continue // e.g. Archive/ subdirs — not indexed
			}
			srcTree.byPkg[strings.TrimSuffix(path.Base(sub), ".tar.gz")] = k
		case strings.HasPrefix(sub, "bin/") && (strings.HasSuffix(sub, ".zip") || strings.HasSuffix(sub, ".tgz")):
			platform, rver, file, ok := parseBinPath(sub)
			if !ok {
				continue
			}
			ns := h.binNS(c, platform, rver)
			t := trees[ns]
			if t == nil {
				t = &tree{ns: ns, dir: c.Repo.Name + "/" + path.Dir(sub) + "/", byPkg: map[string]string{}}
				trees[ns] = t
			}
			pkg, ver, ok := parsePkgFilename(file)
			if !ok {
				continue
			}
			t.byPkg[pkg+"_"+ver] = k
		}
	}

	for _, t := range trees {
		recKeys, lerr := c.Meta.List(t.ns)
		if lerr != nil {
			continue
		}
		recorded := make(map[string]bool, len(recKeys))
		for _, rk := range recKeys {
			var rec pkgRecord
			if ok, _ := c.Meta.GetJSON(t.ns, rk, &rec); !ok {
				continue
			}
			res.MetaChecked++
			pkgKey := rec.Package + "_" + rec.Version
			recorded[pkgKey] = true
			blobKey, ok := t.byPkg[pkgKey]
			if !ok {
				res.Add(integrity.KindMissing, t.dir+pkgKey, rec.Package, rec.Version,
					"package record exists but the tarball blob is gone — it is listed in PACKAGES but installs will 404")
				continue
			}
			if mode == integrity.ModeFull && (strings.HasSuffix(blobKey, ".tar.gz") || strings.HasSuffix(blobKey, ".tgz")) {
				if gerr := readGzipToEOF(c, blobKey, &res.BytesRead); gerr != nil {
					res.Add(integrity.KindMismatch, blobKey, rec.Package, rec.Version,
						"corrupt gzip stream (CRC check failed): "+gerr.Error()+" — bytes changed since publish")
				}
			}
		}
		for pkgKey, blobKey := range t.byPkg {
			if !recorded[pkgKey] {
				pkg, ver, _ := strings.Cut(pkgKey, "_")
				res.Add(integrity.KindOrphan, blobKey, pkg, ver,
					"tarball blob has no package record — it is absent from the PACKAGES index")
			}
		}
	}
	return res, nil
}

// readGzipToEOF streams a gzip blob through decompression to EOF, which
// validates the container's CRC32 of the uncompressed data — CRAN records
// store no digest, so the gzip checksum is the corruption detector.
func readGzipToEOF(c *format.Context, key string, bytesRead *int64) error {
	rc, err := c.Blob.Get(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &countingReader{r: rc}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		*bytesRead += cr.n
		return err
	}
	defer gz.Close()
	// #nosec G110 -- output goes to io.Discard (no allocation); this is the CRC integrity check of an already-stored, upload-size-limited CRAN artifact
	_, err = io.Copy(io.Discard, gz)
	*bytesRead += cr.n
	if err != nil {
		return err
	}
	return gz.Close()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
