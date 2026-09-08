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
//
// SizeBytes is the EXCLUSIVE size: the manifest plus the blobs no other tagged
// manifest references. Summing the manifest alone would be wildly misleading —
// it is a couple of kilobytes in front of layers that may be hundreds of
// megabytes — and summing every referenced blob would double-count layers
// shared between tags, promising space that deleting one tag cannot free.
// Under-promising is the safe direction for "how much will this reclaim".
func (h *Handler) ListVersions(c *format.Context) ([]format.Version, error) {
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}

	// One pass over every tagged manifest gives a reference count per digest,
	// so exclusivity is a map lookup rather than a walk per tag.
	type tagRef struct{ image, tag, digest string }
	var tags []tagRef
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
		tags = append(tags, tagRef{image, tag, dgst})
	}
	refs := map[string]int{}
	perTag := map[string][]string{}
	for _, t := range tags {
		blobs := h.manifestRefs(c, h.manifestKey(c, t.digest))
		perTag[t.digest] = blobs
		for _, d := range blobs {
			refs[d]++
		}
	}

	out := make([]format.Version, 0, len(tags))
	for _, t := range tags {
		size := h.blobSize(c, h.manifestKey(c, t.digest))
		for _, d := range perTag[t.digest] {
			if refs[d] == 1 {
				size += h.blobSize(c, h.blobKey(c, d))
			}
		}
		out = append(out, format.Version{
			Component:   t.image,
			Version:     t.tag,
			PublishedAt: h.tagPushTime(c, t.image, t.tag),
			BlobKeys:    []string{h.manifestKey(c, t.digest)},
			SizeBytes:   size,
		})
	}
	return out, nil
}

func (h *Handler) blobSize(c *format.Context, key string) int64 {
	if info, ok, _ := c.Blob.Stat(key); ok {
		return info.Size
	}
	return 0
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

// DeleteVersions implements format.Handler: removes a batch of tags and sweeps
// once, instead of recomputing which blobs are still reachable after every
// single tag. Pruning a thousand tags off one image used to walk every surviving
// manifest a thousand times.
func (h *Handler) DeleteVersions(c *format.Context, versions []format.Version) (int64, error) {
	if len(versions) == 0 {
		return 0, nil
	}

	// Drop the tags first, so "still tagged" below reflects the whole batch.
	doomed := map[string]bool{} // manifest digests whose tags are going
	for _, v := range versions {
		var dgst string
		if ok, _ := c.Meta.GetJSON(h.ns(c), "tags/"+v.Component+"/"+v.Version, &dgst); !ok || dgst == "" {
			continue
		}
		c.Meta.Delete(h.ns(c), "tags/"+v.Component+"/"+v.Version)      //nolint:errcheck
		c.Meta.Delete(h.ns(c), "tag-times/"+v.Component+"/"+v.Version) //nolint:errcheck
		doomed[dgst] = true
	}
	if len(doomed) == 0 {
		return 0, nil
	}

	// One reachability pass for the whole batch. A manifest another tag still
	// points at is not orphaned, however many tags of its own were removed.
	surviving := h.taggedDigests(c)
	keep := map[string]bool{}
	for d := range surviving {
		h.markReachable(c, d, keep)
	}

	var freed int64
	for dgst := range doomed {
		if surviving[dgst] || keep[dgst] {
			continue
		}
		mk := h.manifestKey(c, dgst)
		for _, ref := range h.manifestRefs(c, mk) {
			if keep[ref] || doomed[ref] {
				continue
			}
			freed += h.deleteBlobIfPresent(c, h.blobKey(c, ref))
		}
		freed += h.deleteBlobIfPresent(c, mk)
		c.Meta.Delete(h.ns(c), "manifests/"+dgst) //nolint:errcheck
	}
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
