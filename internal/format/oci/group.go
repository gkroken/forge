package oci

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"forge/internal/format"
	"forge/internal/repo"
)

// Group mode: several registries behind one pull URL, so `docker pull` can
// reach internal images and upstream ones without the client knowing which is
// which.
//
// The merge policy lives in format.GroupMerge and the routing in
// format.GroupFetch — neither has anything OCI-specific in it. Only tags/list
// is a real merge; manifests and blobs are served by whichever member holds
// them.

// serveGroup handles the read-only surface of a group registry.
func (h *Handler) serveGroup(w http.ResponseWriter, r *http.Request, c *format.Context, image, op string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		ociError(w, "UNSUPPORTED", "group repositories are read-only: push to the hosted member",
			http.StatusMethodNotAllowed)
		return
	}
	switch op {
	// A blob is addressed by its own digest, so which member answers changes
	// latency, never correctness — no shadowing applies, and restricting it
	// would break a hosted manifest whose layers are cached in a proxy member.
	case "blobs":
		if !format.GroupFetch(h, w, r, c) {
			ociError(w, "BLOB_UNKNOWN", "blob not found in any group member", http.StatusNotFound)
		}

	// Manifests are name-addressed, so ownership applies: see hostedOwned.
	case "manifests":
		if !format.GroupFetch(h, w, r, h.ownershipCtx(c, image)) {
			ociError(w, "MANIFEST_UNKNOWN", "manifest not found in any group member", http.StatusNotFound)
		}

	case "tags/list":
		h.groupTags(w, h.ownershipCtx(c, image), image)

	default:
		ociError(w, "UNSUPPORTED", "unknown OCI operation", http.StatusNotFound)
	}
}

// ownershipCtx applies image-name ownership: once a hosted member holds an
// image name, proxy members are excluded from answering for that name at all.
//
// The alternative — shadowing only the merged tag list — is worse than either
// pure policy, because `docker pull` resolves a tag directly and never reads
// tags/list. The listing would hide an upstream tag that still pulls fine.
// Excluding proxy members from both reads is what makes the hiding real, and
// it matches how npm, PyPI, CRAN and Helm already treat a name their hosted
// member owns.
//
// Blobs are deliberately exempt: they are addressed by digest, and a hosted
// manifest may legitimately reference layers a proxy member has cached.
func (h *Handler) ownershipCtx(c *format.Context, image string) *format.Context {
	if !h.hostedMemberOwns(c, image) {
		return c
	}
	gc := *c
	outer := c.MemberFilter
	gc.MemberFilter = func(m repo.Repository) bool {
		if outer != nil && !outer(m) {
			return false
		}
		return m.Kind != repo.Proxy
	}
	return &gc
}

// hostedMemberOwns reports whether any non-proxy member holds this image name.
func (h *Handler) hostedMemberOwns(c *format.Context, image string) bool {
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok || mc.Repo.Kind == repo.Proxy {
			continue
		}
		keys, _ := mc.Meta.List(h.ns(mc))
		for _, k := range keys {
			if strings.HasPrefix(k, "tags/"+image+"/") {
				return true
			}
		}
	}
	return false
}

// imageTag is one entry of a merged tag list. It exists so GroupMerge has a
// record type to apply the group policy to.
type imageTag struct {
	Image string
	Tag   string
}

// groupTags merges every member's tag list for an image. This is the one OCI
// read where a hosted member can hide a proxy member: an internal
// "myorg/app:latest" must win over a public image of the same name, which is
// the container-image form of the dependency-confusion rule the other formats
// already enforce on their indexes.
func (h *Handler) groupTags(w http.ResponseWriter, c *format.Context, image string) {
	merged := format.GroupMerge(c,
		func(mc *format.Context) []imageTag { return h.memberTags(mc, image) },
		func(t imageTag) (string, string) { return t.Image, t.Tag })

	tags := make([]string, 0, len(merged))
	for _, t := range merged {
		tags = append(tags, t.Tag)
	}
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"name": image, "tags": tags}) //nolint:errcheck
}

// memberTags asks one member which tags it has for an image. A hosted member
// reads its own records; a proxy member has to ask upstream, because it caches
// only what has been pulled so far.
func (h *Handler) memberTags(mc *format.Context, image string) []imageTag {
	var names []string
	if mc.Repo.Kind == repo.Proxy {
		names = h.upstreamTags(mc, image)
	} else {
		keys, _ := mc.Meta.List(h.ns(mc))
		prefix := "tags/" + image + "/"
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				names = append(names, strings.TrimPrefix(k, prefix))
			}
		}
	}
	out := make([]imageTag, 0, len(names))
	for _, n := range names {
		out = append(out, imageTag{Image: image, Tag: n})
	}
	return out
}
