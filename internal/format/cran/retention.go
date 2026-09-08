package cran

import (
	"forge/internal/format"
)

// Retention. A package record in "{repo}:cran" is keyed "{pkg}_{version}" and
// carries the upload time; the source tarball is the version's single artifact.

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	out := make([]format.Version, 0, len(keys))
	for _, k := range keys {
		var rec pkgRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); !ok {
			continue
		}
		out = append(out, format.Version{
			Component:   rec.Package,
			Version:     rec.Version,
			PublishedAt: rec.UploadedAt,
			BlobKeys:    []string{h.tarballKey(c, rec.Package, rec.Version)},
		})
	}
	return out, nil
}

// DeleteVersion implements format.Handler: removes the source tarball and its
// record, so the generated PACKAGES index no longer lists it.
func (h *Handler) DeleteVersion(c *format.Context, pkg, version string) (int64, error) {
	var freed int64
	key := h.tarballKey(c, pkg, version)
	if info, exists, _ := c.Blob.Stat(key); exists {
		freed = info.Size
		c.Blob.Delete(key) //nolint:errcheck
	}
	c.Meta.Delete(h.ns(c), pkg+"_"+version) //nolint:errcheck
	return freed, nil
}

func (h *Handler) tarballKey(c *format.Context, pkg, version string) string {
	return c.Repo.Name + "/src/contrib/" + pkg + "_" + version + ".tar.gz"
}
