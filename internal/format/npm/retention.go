package npm

import (
	"forge/internal/format"
	"strings"
)

// Retention. The version records live in "{repo}:npm:v" keyed "{pkg}:{version}",
// and the tarball is the single artifact making up a version. Publish time is
// not kept here — npm's records are rebuilt from key names — so PublishedAt is
// left zero and retention resolves it from the publish ledger.

func (h *Handler) versionsNS(c *format.Context) string { return c.Repo.Name + ":npm:v" }

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Meta.List(h.versionsNS(c))
	if err != nil {
		return nil, err
	}
	out := make([]format.Version, 0, len(keys))
	for _, k := range keys {
		pkg, ver, ok := strings.Cut(k, ":")
		if !ok {
			continue
		}
		out = append(out, format.Version{
			Component: pkg,
			Version:   ver,
			BlobKeys:  []string{h.tarballKey(c, pkg, ver)},
		})
	}
	return out, nil
}

// DeleteVersion implements format.Handler: removes the tarball, the version
// record, and the version's entry in the packument, so a subsequent install
// resolves against what is actually stored.
func (h *Handler) DeleteVersion(c *format.Context, pkg, version string) (int64, error) {
	var freed int64
	key := h.tarballKey(c, pkg, version)
	if info, exists, _ := c.Blob.Stat(key); exists {
		freed = info.Size
		c.Blob.Delete(key) //nolint:errcheck
	}
	c.Meta.Delete(h.versionsNS(c), pkg+":"+version) //nolint:errcheck

	var packument map[string]any
	if ok, _ := c.Meta.GetJSON(h.ns(c), pkg, &packument); ok {
		if vers, ok := packument["versions"].(map[string]any); ok {
			delete(vers, version)
			packument["versions"] = vers
		}
		c.Meta.PutJSON(h.ns(c), pkg, packument) //nolint:errcheck
	}
	return freed, nil
}

// tarballKey is where publish() puts the tarball. The FILENAME uses only the
// last path segment, so a scoped package "@acme/tool" is stored at
// "{repo}/@acme/tool/-/tool-1.0.0.tgz" — not ".../@acme/tool-1.0.0.tgz".
// Getting this wrong means retention deletes the record and leaves the tarball
// on disk forever, and scoped packages are most of npm.
func (h *Handler) tarballKey(c *format.Context, pkg, version string) string {
	return c.Repo.Name + "/" + pkg + "/-/" + lastPathSeg(pkg) + "-" + version + ".tgz"
}
