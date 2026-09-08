package maven

import (
	"strings"
	"time"

	"forge/internal/format"
)

// Retention. Maven has no per-version meta record for releases — the blob layout
// IS the index — so versions are enumerated by walking blob keys and grouping on
// the path above the version directory, which is exactly "{groupId}/{artifactId}".
//
// Snapshots additionally carry a deploy timestamp in the snapshot namespace;
// that is the only publish time maven keeps of its own, so it is reported when
// present and the publish ledger fills the rest.

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Blob.List(c.Repo.Name + "/")
	if err != nil {
		return nil, err
	}
	type key struct{ ga, version string }
	grouped := map[key][]string{}
	var order []key

	prefix := c.Repo.Name + "/"
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		parts := strings.Split(rel, "/")
		if len(parts) < 3 {
			continue // need at least groupId/artifactId/version/file
		}
		gk := key{ga: strings.Join(parts[:len(parts)-2], "/"), version: parts[len(parts)-2]}
		if _, seen := grouped[gk]; !seen {
			order = append(order, gk)
		}
		grouped[gk] = append(grouped[gk], k)
	}

	out := make([]format.Version, 0, len(order))
	for _, gk := range order {
		out = append(out, format.Version{
			Component:   gk.ga,
			Version:     gk.version,
			PublishedAt: h.snapshotUploadTime(c, gk.version, grouped[gk]),
			BlobKeys:    grouped[gk],
		})
	}
	return out, nil
}

// DeleteVersion implements format.Handler: removes every artifact under the
// version directory, plus the snapshot records describing them.
//
// Maven components are spelled two ways in forge — "groupId:artifactId" by the
// admin and browse APIs, "groupId/artifactId" by the blob layout that
// ListVersions reports. Both are accepted, because a caller holding one has no
// way to know which this method wants.
func (h *Handler) DeleteVersion(c *format.Context, component, version string) (int64, error) {
	ga := gaPath(component)
	keys, err := c.Blob.List(c.Repo.Name + "/" + ga + "/" + version + "/")
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, k := range keys {
		if info, exists, _ := c.Blob.Stat(k); exists {
			freed += info.Size
			c.Blob.Delete(k) //nolint:errcheck
		}
	}
	h.forgetSnapshotMeta(c, ga, version)
	return freed, nil
}

// mavenPathParts splits a maven sub-path into its component and version, the
// same way ListVersions groups blobs. Returns false for a path too short to
// carry a version (a metadata file at the artifact level, say).
func mavenPathParts(sub string) (component, version string, ok bool) {
	parts := strings.Split(strings.Trim(sub, "/"), "/")
	if len(parts) < 3 {
		return "", "", false
	}
	return strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], true
}

// gaPath normalises a maven component to its blob-layout form. "com.acme:app"
// becomes "com/acme/app"; a value already in path form is returned unchanged.
func gaPath(component string) string {
	group, artifact, ok := strings.Cut(component, ":")
	if !ok {
		return component
	}
	return strings.ReplaceAll(group, ".", "/") + "/" + artifact
}

// snapshotUploadTime returns the earliest deploy time recorded for a snapshot
// version, or the zero time for a release (which has no such record).
func (h *Handler) snapshotUploadTime(c *format.Context, version string, blobKeys []string) time.Time {
	ns := h.snapVersNS(c)
	allKeys, _ := c.Meta.List(ns)
	for _, bk := range blobKeys {
		prefix := strings.TrimPrefix(strings.TrimPrefix(bk, c.Repo.Name+"/"), "/")
		for _, mk := range allKeys {
			if !strings.HasPrefix(mk, prefix+":") {
				continue
			}
			var rec struct {
				UploadedAt time.Time `json:"uploadedAt"`
			}
			if ok, _ := c.Meta.GetJSON(ns, mk, &rec); ok && !rec.UploadedAt.IsZero() {
				return rec.UploadedAt
			}
		}
	}
	return time.Time{}
}

// forgetSnapshotMeta drops the snapshot records for a deleted version so they do
// not linger as integrity orphans.
func (h *Handler) forgetSnapshotMeta(c *format.Context, ga, version string) {
	ns := h.snapVersNS(c)
	keys, _ := c.Meta.List(ns)
	want := ga + "/" + version + "/"
	for _, k := range keys {
		if strings.HasPrefix(k, want) {
			c.Meta.Delete(ns, k) //nolint:errcheck
		}
	}
}
