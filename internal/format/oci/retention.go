package oci

import (
	"encoding/json"
	"strings"
	"time"

	"forge/internal/format"
)

// Retention. The unit is a tag: image:tag is what a person publishes and reasons
// about. Untagged manifests are not retention's business — they are either
// referenced by a tagged index, or debris for integrity verify to report.
//
// Deleting a tag frees nothing by itself, since the bytes live in the manifest
// and its config and layer blobs, so DeleteVersion also sweeps. The sweep is
// deliberately narrow: it drops only what the removed manifest referenced and no
// surviving manifest still needs. A registry-wide mark-and-sweep would also
// collect the layers of an in-flight `docker push`, which uploads blobs before
// the manifest referencing them; scoping the sweep to what this deletion
// orphaned makes that impossible, so retention needs no read-only window.

// ListVersions implements format.Handler.
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	var out []format.Version
	for _, k := range keys {
		if !strings.HasPrefix(k, "tags/") {
			continue
		}
		image, tag, ok := lastCut(strings.TrimPrefix(k, "tags/"), "/")
		if !ok {
			continue
		}
		var dgst string
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &dgst); !ok || dgst == "" {
			continue
		}
		out = append(out, format.Version{
			Component:   image,
			Version:     tag,
			PublishedAt: h.tagPushTime(c, image, tag),
			BlobKeys:    []string{h.manifestKey(c, dgst)},
		})
	}
	return out, nil
}

// DeleteVersion implements format.Handler: removes the tag, then the manifest
// and any blobs it alone was keeping alive.
func (h *Handler) DeleteVersion(c *format.Context, image, tag string) (int64, error) {
	var dgst string
	if ok, _ := c.Meta.GetJSON(h.ns(c), "tags/"+image+"/"+tag, &dgst); !ok || dgst == "" {
		return 0, nil
	}
	c.Meta.Delete(h.ns(c), "tags/"+image+"/"+tag)      //nolint:errcheck
	c.Meta.Delete(h.ns(c), "tag-times/"+image+"/"+tag) //nolint:errcheck

	// Still referenced by another tag? Then nothing is orphaned.
	surviving := h.taggedDigests(c)
	if surviving[dgst] {
		return 0, nil
	}

	keep := map[string]bool{}
	for d := range surviving {
		h.markReachable(c, d, keep)
	}
	var freed int64
	mk := h.manifestKey(c, dgst)
	for _, ref := range h.manifestRefs(c, mk) {
		if keep[ref] {
			continue
		}
		freed += h.deleteBlobIfPresent(c, h.blobKey(c, ref))
	}
	freed += h.deleteBlobIfPresent(c, mk)
	c.Meta.Delete(h.ns(c), "manifests/"+dgst) //nolint:errcheck
	return freed, nil
}

// taggedDigests is the set of manifest digests any tag still points at.
func (h *Handler) taggedDigests(c *format.Context) map[string]bool {
	out := map[string]bool{}
	keys, _ := c.Meta.List(h.ns(c))
	for _, k := range keys {
		if !strings.HasPrefix(k, "tags/") {
			continue
		}
		var d string
		if ok, _ := c.Meta.GetJSON(h.ns(c), k, &d); ok && d != "" {
			out[d] = true
		}
	}
	return out
}

func (h *Handler) tagPushTime(c *format.Context, image, tag string) time.Time {
	var ts string
	if ok, _ := c.Meta.GetJSON(h.ns(c), "tag-times/"+image+"/"+tag, &ts); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// manifestRefs returns the digests a manifest points at: its config, its layers,
// and for an index, its child manifests.
func (h *Handler) manifestRefs(c *format.Context, key string) []string {
	rc, err := c.Blob.Get(key)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if json.NewDecoder(rc).Decode(&m) != nil {
		return nil
	}
	var out []string
	if m.Config.Digest != "" {
		out = append(out, m.Config.Digest)
	}
	for _, l := range m.Layers {
		out = append(out, l.Digest)
	}
	for _, sm := range m.Manifests {
		out = append(out, sm.Digest)
	}
	return out
}

func (h *Handler) markReachable(c *format.Context, digest string, keep map[string]bool) {
	if keep[digest] {
		return
	}
	keep[digest] = true
	for _, ref := range h.manifestRefs(c, h.manifestKey(c, digest)) {
		if !keep[ref] {
			keep[ref] = true
			h.markReachable(c, ref, keep) // an index's children are manifests too
		}
	}
}

func (h *Handler) deleteBlobIfPresent(c *format.Context, key string) int64 {
	info, exists, _ := c.Blob.Stat(key)
	if !exists {
		return 0
	}
	c.Blob.Delete(key) //nolint:errcheck
	return info.Size
}

// lastCut splits on the FINAL separator, so an image name containing slashes
// ("acme/api") keeps its path and only the tag comes off the end.
func lastCut(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+len(sep):], true
}
