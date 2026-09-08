package helm

import (
	"forge/internal/format"
)

// Retention. A chart record in "{repo}:helm" carries the filename of its single
// artifact and the upload time, so helm answers both without consulting the
// publish ledger.

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	out := make([]format.Version, 0, len(keys))
	for _, k := range keys {
		var rec chartRecord
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &rec); !ok {
			continue
		}
		out = append(out, format.Version{
			Component:   rec.Name,
			Version:     rec.Version,
			PublishedAt: rec.UploadedAt,
			BlobKeys:    []string{c.Repo.Name + "/" + rec.Filename},
		})
	}
	return out, nil
}

// DeleteVersion implements format.Handler: removes the chart archive and its
// record, so the generated index.yaml no longer lists it.
func (h *Handler) DeleteVersion(c *format.Context, chart, version string) (int64, error) {
	var rec chartRecord
	key := chart + "-" + version
	if ok, _ := c.Meta.GetJSON(h.ns(c), key, &rec); !ok {
		return 0, nil
	}
	var freed int64
	blobKey := c.Repo.Name + "/" + rec.Filename
	if info, exists, _ := c.Blob.Stat(blobKey); exists {
		freed = info.Size
		c.Blob.Delete(blobKey) //nolint:errcheck
	}
	c.Meta.Delete(h.ns(c), key) //nolint:errcheck
	return freed, nil
}
