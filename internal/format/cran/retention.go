package cran

import (
	"path"
	"sort"
	"strings"
	"time"

	"forge/internal/format"
)

// Retention. A CRAN version is the package at that version across ALL its
// artifacts: the source tarball under src/contrib, plus every platform/R-version
// binary under bin/. Keeping "the last 3 versions of ggplot2" has to mean three
// versions everywhere, or the binaries — which are the bulky ones for an R
// shop — accumulate forever while the source index looks tidy.
//
// Binaries are discovered from the blob side because meta.Store cannot enumerate
// namespaces, and the platform and R version are encoded in the path:
//
//	bin/{platform}/contrib/{rver}/{pkg}_{version}.{ext}

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	type key struct{ pkg, version string }
	blobs := map[key][]string{}
	published := map[key]time.Time{}

	// Source packages carry the publish time in their record.
	srcKeys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	for _, k := range srcKeys {
		var rec pkgRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); !ok {
			continue
		}
		kk := key{rec.Package, rec.Version}
		blobs[kk] = append(blobs[kk], h.tarballKey(c, rec.Package, rec.Version))
		if !rec.UploadedAt.IsZero() {
			published[kk] = rec.UploadedAt.UTC()
		}
	}

	// Binaries: every blob under bin/ belongs to some package+version.
	binKeys, _ := c.Blob.List(c.Repo.Name + "/bin/")
	for _, bk := range binKeys {
		pkg, ver, ok := cranBinaryParts(bk)
		if !ok {
			continue
		}
		kk := key{pkg, ver}
		blobs[kk] = append(blobs[kk], bk)
	}

	out := make([]format.Version, 0, len(blobs))
	for kk, keys := range blobs {
		v := format.Version{Component: kk.pkg, Version: kk.version, BlobKeys: keys}
		if t, ok := published[kk]; ok {
			v.PublishedAt = t
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// DeleteVersion implements format.Handler: removes the source tarball and every
// platform binary for this version, plus the meta records describing them.
func (h *Handler) DeleteVersion(c *format.Context, pkg, version string) (int64, error) {
	var freed int64

	src := h.tarballKey(c, pkg, version)
	if info, exists, _ := c.Blob.Stat(src); exists {
		freed += info.Size
		c.Blob.Delete(src) //nolint:errcheck
	}
	c.Meta.Delete(h.ns(c), pkg+"_"+version) //nolint:errcheck

	binKeys, _ := c.Blob.List(c.Repo.Name + "/bin/")
	for _, bk := range binKeys {
		bp, bv, ok := cranBinaryParts(bk)
		if !ok || bp != pkg || bv != version {
			continue
		}
		if info, exists, _ := c.Blob.Stat(bk); exists {
			freed += info.Size
			c.Blob.Delete(bk) //nolint:errcheck
		}
		if platform, rver, ok := cranBinaryLocation(c, bk); ok {
			c.Meta.Delete(h.binNS(c, platform, rver), pkg+"_"+version) //nolint:errcheck
		}
	}
	return freed, nil
}

// cranBinaryParts pulls the package and version out of a binary blob key, whose
// filename is "{pkg}_{version}.{ext}" (.zip on Windows, .tgz on macOS).
func cranBinaryParts(blobKey string) (pkg, version string, ok bool) {
	base := path.Base(blobKey)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(base, ext) {
			base = strings.TrimSuffix(base, ext)
			break
		}
	}
	return strings.Cut(base, "_")
}

// cranBinaryLocation recovers the platform and R version from a binary blob key
// shaped "{repo}/bin/{platform}/contrib/{rver}/{file}". The platform itself may
// contain a slash ("macosx/big-sur-arm64"), so it is whatever sits between
// "bin/" and "/contrib/".
func cranBinaryLocation(c *format.Context, blobKey string) (platform, rver string, ok bool) {
	rest, ok := strings.CutPrefix(blobKey, c.Repo.Name+"/bin/")
	if !ok {
		return "", "", false
	}
	platform, after, ok := strings.Cut(rest, "/contrib/")
	if !ok {
		return "", "", false
	}
	rver, _, ok = strings.Cut(after, "/")
	return platform, rver, ok
}

func (h *Handler) tarballKey(c *format.Context, pkg, version string) string {
	return c.Repo.Name + "/src/contrib/" + pkg + "_" + version + ".tar.gz"
}
