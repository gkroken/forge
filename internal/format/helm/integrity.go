package helm

import (
	"fmt"
	"strings"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/repo"
)

// Compile-time assertion that the handler implements the integrity seam.

// VerifyIntegrity implements format.Handler.
//
// Helm's source of truth is the chartRecord in "{repo}:helm" (index.yaml is
// generated from it); the .tgz bytes live in the blob store and every record
// stores the sha256 digest computed at upload. The invariants:
//
//   - chart record whose .tgz blob is gone            → missing
//   - .tgz blob no chart record claims                → orphan
//   - full mode: blob sha256 ≠ record digest          → mismatch
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	if c.Repo.Kind == repo.Proxy {
		return integrity.VerifyProxyCache(c.Repo.Name, c.Blob, c.Meta)
	}
	var res integrity.Result
	if c.Repo.Kind != repo.Hosted {
		return res, nil
	}

	claimed := map[string]bool{} // blob keys owned by a chart record
	recKeys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return res, err
	}
	for _, rk := range recKeys {
		var rec chartRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), rk, &rec); !ok {
			continue
		}
		res.MetaChecked++
		key := c.Key(rec.Filename)
		claimed[key] = true
		_, exists, serr := c.Blob.Stat(key)
		if serr != nil {
			continue
		}
		if !exists {
			res.Add(integrity.KindMissing, key, rec.Name, rec.Version,
				"chart record exists but the .tgz blob is gone — the chart appears in index.yaml but pulls will 404")
			continue
		}
		if mode != integrity.ModeFull || rec.Digest == "" {
			continue
		}
		hs, herr := integrity.HashBlob(c.Blob, key)
		if herr != nil {
			res.Add(integrity.KindMismatch, key, rec.Name, rec.Version, "chart unreadable: "+herr.Error())
			continue
		}
		res.BytesRead += hs.Size
		if !strings.EqualFold(hs.SHA256, rec.Digest) {
			res.Add(integrity.KindMismatch, key, rec.Name, rec.Version,
				fmt.Sprintf("record digest is %s but chart hashes to %s — bytes changed since upload", rec.Digest, hs.SHA256))
		}
	}

	blobs, err := c.Blob.List(c.Repo.Name + "/")
	if err != nil {
		return res, err
	}
	res.BlobsChecked = len(blobs)
	for _, k := range blobs {
		if !strings.HasSuffix(k, ".tgz") {
			continue
		}
		if !claimed[k] {
			name, ver := chartNameVer(strings.TrimPrefix(k, c.Repo.Name+"/"))
			res.Add(integrity.KindOrphan, k, name, ver,
				"chart blob has no record — it is absent from index.yaml and nothing serves it")
		}
	}
	return res, nil
}

// chartNameVer splits "{name}-{version}.tgz" at the last hyphen before a
// digit, best-effort (chart names may themselves contain hyphens).
func chartNameVer(filename string) (string, string) {
	base := strings.TrimSuffix(filename, ".tgz")
	for i := len(base) - 1; i > 0; i-- {
		if base[i] == '-' && i+1 < len(base) && base[i+1] >= '0' && base[i+1] <= '9' {
			return base[:i], base[i+1:]
		}
	}
	return base, ""
}
