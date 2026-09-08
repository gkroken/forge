package npm

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/proxy"
	"forge/internal/repo"
)

// Compile-time assertion that the handler satisfies every format seam. The
// interface requires them all, so this fails to build if one is missing rather
// than silently leaving the feature absent.
var _ format.Handler = (*Handler)(nil)

// Reindex implements format.Handler: it rebuilds the materialized packument
// of every package that has per-version records (synchronously, plus an async
// regen job per package when a queue is wired, mirroring publish). This is
// the repair for "drift" integrity findings.
func (h *Handler) Reindex(ctx context.Context, c *format.Context) (int, error) {
	keys, err := c.Meta.List(h.versNS(c))
	if err != nil {
		return 0, err
	}
	pkgs := map[string]bool{}
	for _, k := range keys {
		if pkg, _, ok := strings.Cut(k, ":"); ok {
			pkgs[pkg] = true
		}
	}
	for pkg := range pkgs {
		h.triggerRegen(ctx, c, pkg)
	}
	return len(pkgs), nil
}

// VerifyIntegrity implements format.Handler.
//
// npm's source of truth is split: per-version records in "{repo}:npm:v" own
// the metadata, tarballs in the blob store own the bytes, and the packument
// in "{repo}:npm" is a materialized index rebuilt from the records. The
// invariants:
//
//   - version record whose tarball blob is gone                    → missing
//   - tarball blob no version record (or old-format packument) claims → orphan
//   - full mode: tarball bytes ≠ dist.shasum / dist.integrity      → mismatch
//   - packument version set ≠ per-version record set               → drift
//   - dist-tag pointing at a version that doesn't exist            → drift
//
// Packages published before per-version records existed live only in the
// packument; for those the packument itself is treated as the record source
// and no drift is reported.
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	if c.Repo.Kind == repo.Proxy {
		return h.verifyProxy(c)
	}
	var res integrity.Result
	if c.Repo.Kind != repo.Hosted {
		return res, nil
	}

	// Version records → expected tarballs.
	versKeys, err := c.Meta.List(h.versNS(c))
	if err != nil {
		return res, err
	}
	recorded := map[string]map[string]bool{} // pkg → version set
	for _, vk := range versKeys {
		pkg, ver, ok := strings.Cut(vk, ":") // npm names cannot contain ":"
		if !ok {
			continue
		}
		var vobj map[string]any
		if ok, _ := c.Meta.GetJSON(h.versNS(c), vk, &vobj); !ok {
			continue
		}
		res.MetaChecked++
		if recorded[pkg] == nil {
			recorded[pkg] = map[string]bool{}
		}
		recorded[pkg][ver] = true

		tarKey := c.Key(pkg + "/-/" + lastPathSeg(pkg) + "-" + ver + ".tgz")
		_, exists, serr := c.Blob.Stat(tarKey)
		if serr != nil {
			continue
		}
		if !exists {
			res.Add(integrity.KindMissing, tarKey, pkg, ver,
				"version record exists but the tarball blob is gone — installs of this version will 404")
			continue
		}
		if mode == integrity.ModeFull {
			h.checkTarballDigest(c, &res, tarKey, pkg, ver, vobj)
		}
	}

	// Packument drift (only for packages that have per-version records).
	packs := map[string]map[string]bool{} // pkg → packument version set
	for pkg, vers := range recorded {
		packVers, ok := h.packumentVersions(c, pkg)
		if !ok {
			res.Add(integrity.KindDrift, h.ns(c)+" · "+pkg, pkg, "",
				"per-version records exist but the materialized packument is missing — reindex rebuilds it")
			continue
		}
		packs[pkg] = packVers
		for ver := range vers {
			if !packVers[ver] {
				res.Add(integrity.KindDrift, h.ns(c)+" · "+pkg, pkg, ver,
					"version has a record but is absent from the packument — reindex rebuilds it")
			}
		}
		for ver := range packVers {
			if !vers[ver] {
				res.Add(integrity.KindDrift, h.ns(c)+" · "+pkg, pkg, ver,
					"packument lists a version with no per-version record — reindex rebuilds it")
			}
		}
	}

	// Dist-tags must point at versions that exist.
	tagPkgs, _ := c.Meta.List(h.tagsNS(c))
	for _, pkg := range tagPkgs {
		var tags map[string]any
		if ok, _ := c.Meta.GetJSON(h.tagsNS(c), pkg, &tags); !ok {
			continue
		}
		res.MetaChecked++
		known := recorded[pkg]
		if known == nil {
			var ok bool
			if known, ok = h.packumentVersions(c, pkg); !ok {
				continue // old-format package with no packument either; nothing to check against
			}
		}
		for tag, v := range tags {
			ver, _ := v.(string)
			if ver != "" && !known[ver] {
				res.Add(integrity.KindDrift, h.tagsNS(c)+" · "+pkg, pkg, ver,
					fmt.Sprintf("dist-tag %q points at version %s which does not exist", tag, ver))
			}
		}
	}

	// Orphan tarballs: blobs no record (or old-format packument) claims.
	blobs, err := c.Blob.List(c.Repo.Name + "/")
	if err != nil {
		return res, err
	}
	res.BlobsChecked = len(blobs)
	for _, k := range blobs {
		sub := strings.TrimPrefix(k, c.Repo.Name+"/")
		i := strings.Index(sub, "/-/")
		if i < 0 || !strings.HasSuffix(sub, ".tgz") {
			continue
		}
		pkg := sub[:i]
		ver := strings.TrimPrefix(strings.TrimSuffix(sub[i+3:], ".tgz"), lastPathSeg(pkg)+"-")
		known := recorded[pkg]
		if known == nil {
			known, _ = h.packumentVersions(c, pkg)
		}
		if !known[ver] {
			res.Add(integrity.KindOrphan, k, pkg, ver,
				"tarball blob has no version record and no packument entry — nothing serves it")
		}
	}
	return res, nil
}

// checkTarballDigest re-hashes one tarball and compares it against the
// version record's dist.shasum (sha1) or dist.integrity (sha512) expectation.
func (h *Handler) checkTarballDigest(c *format.Context, res *integrity.Result, tarKey, pkg, ver string, vobj map[string]any) {
	dist, _ := vobj["dist"].(map[string]any)
	shasum, _ := dist["shasum"].(string)
	sri, _ := dist["integrity"].(string)
	if shasum == "" && !strings.HasPrefix(sri, "sha512-") {
		return // no stored expectation to verify against
	}
	hs, err := integrity.HashBlob(c.Blob, tarKey)
	if err != nil {
		res.Add(integrity.KindMismatch, tarKey, pkg, ver, "tarball unreadable: "+err.Error())
		return
	}
	res.BytesRead += hs.Size
	if shasum != "" {
		if !strings.EqualFold(shasum, hs.SHA1) {
			res.Add(integrity.KindMismatch, tarKey, pkg, ver,
				fmt.Sprintf("dist.shasum says %s but tarball hashes to %s — bytes changed since publish", shasum, hs.SHA1))
		}
		return
	}
	got := "sha512-" + base64.StdEncoding.EncodeToString(hs.SHA512)
	if got != sri {
		res.Add(integrity.KindMismatch, tarKey, pkg, ver,
			"dist.integrity does not match the tarball bytes — changed since publish")
	}
}

// packumentVersions returns the version set of a stored packument.
func (h *Handler) packumentVersions(c *format.Context, pkg string) (map[string]bool, bool) {
	var pack struct {
		Versions map[string]any `json:"versions"`
	}
	ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &pack)
	if !ok {
		return nil, false
	}
	set := make(map[string]bool, len(pack.Versions))
	for v := range pack.Versions {
		set[v] = true
	}
	return set, true
}

// verifyProxy checks a proxy repo: tarballs follow the shared blob-keyed
// cache convention; packuments are cached under their package name in
// "{repo}:proxy" with the document itself in "{repo}:npm".
func (h *Handler) verifyProxy(c *format.Context) (integrity.Result, error) {
	res, err := integrity.VerifyProxyCache(c.Repo.Name, c.Blob, c.Meta)
	if err != nil {
		return res, err
	}
	keys, err := c.Meta.List(h.proxyNS(c))
	if err != nil {
		return res, err
	}
	for _, k := range keys {
		if k == proxy.HealthKey || strings.HasPrefix(k, c.Repo.Name+"/") {
			continue // blob-keyed entries already handled by VerifyProxyCache
		}
		var ce proxy.CacheEntry
		if ok, _ := c.Meta.GetJSON(h.proxyNS(c), k, &ce); !ok {
			continue
		}
		res.MetaChecked++
		if ce.NotFound {
			continue
		}
		var doc map[string]any
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &doc); !ok {
			res.Add(integrity.KindMissing, h.ns(c)+" · "+k, k, "",
				"packument cache entry exists but the cached packument document is gone (will re-fetch on demand)")
		}
	}
	return res, nil
}
