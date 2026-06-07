// Package oci implements the OCI Distribution Specification v1.0
// (https://github.com/opencontainers/distribution-spec), providing a hosted
// container/artifact registry that works with docker, helm, oras, and any
// other OCI-compliant client.
//
// URL layout (mounted at /v2/{repo-name}/ by server.go):
//
//	HEAD/GET  {image}/blobs/{digest}                -> pull blob
//	POST      {image}/blobs/uploads/                -> initiate upload
//	PATCH     {image}/blobs/uploads/{uuid}          -> stream chunk
//	PUT       {image}/blobs/uploads/{uuid}?digest=  -> finalize upload
//	DELETE    {image}/blobs/{digest}                -> delete blob
//	HEAD/GET  {image}/manifests/{reference}         -> pull manifest
//	PUT       {image}/manifests/{reference}         -> push manifest
//	DELETE    {image}/manifests/{reference}         -> delete manifest
//	GET       {image}/tags/list                     -> list tags
//
// Blobs are stored in the blob store at "{repo}/blobs/{digest}".
// Manifests are stored at "{repo}/manifests/{digest}".
// In-progress uploads are buffered at "{repo}/uploads/{uuid}".
// Tags and manifest media types are tracked in the meta store.
package oci

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"forge/internal/format"
	"forge/internal/repo"
)

// Handler implements format.Handler for the OCI Distribution Spec.
type Handler struct{}

func New() *Handler            { return &Handler{} }
func (h *Handler) Format() string { return "oci" }

// manifestMeta is stored in the meta store (key: "manifests/{digest}")
// to track the media type for each pushed manifest.
type manifestMeta struct {
	MediaType string `json:"mediaType"`
	ImageName string `json:"imageName"`
}

// uploadMeta tracks in-progress uploads (key: "uploads/{uuid}").
type uploadMeta struct {
	ImageName string `json:"imageName"`
}

// --- Serve -----------------------------------------------------------------

func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, c *format.Context) {
	image, op, ref, ok := parseOCISub(c.Sub)
	if !ok {
		ociError(w, "NAME_INVALID", "invalid OCI path: "+c.Sub, http.StatusBadRequest)
		return
	}

	if c.Repo.Kind == repo.Proxy {
		// Proxy mode: pass-through to upstream for GET/HEAD, reject writes.
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.proxyPass(w, r, c, image, op, ref)
		default:
			ociError(w, "UNSUPPORTED", "writes not supported on proxy repositories", http.StatusMethodNotAllowed)
		}
		return
	}

	switch op {
	case "manifests":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.getManifest(w, r, c, image, ref)
		case http.MethodPut:
			h.putManifest(w, r, c, image, ref)
		case http.MethodDelete:
			h.deleteManifest(w, c, image, ref)
		default:
			ociError(w, "UNSUPPORTED", "method not allowed", http.StatusMethodNotAllowed)
		}

	case "blobs":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.getBlob(w, r, c, digest(ref))
		case http.MethodDelete:
			h.deleteBlob(w, c, digest(ref))
		default:
			ociError(w, "UNSUPPORTED", "method not allowed", http.StatusMethodNotAllowed)
		}

	case "blobs/uploads":
		switch r.Method {
		case http.MethodPost:
			h.initiateUpload(w, r, c, image)
		case http.MethodPatch:
			h.patchUpload(w, r, c, image, ref)
		case http.MethodPut:
			h.finalizeUpload(w, r, c, image, ref)
		default:
			ociError(w, "UNSUPPORTED", "method not allowed", http.StatusMethodNotAllowed)
		}

	case "tags/list":
		h.listTags(w, c, image)

	default:
		ociError(w, "UNSUPPORTED", "unknown OCI operation", http.StatusNotFound)
	}
}

// --- blob endpoints --------------------------------------------------------

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, c *format.Context, dgst string) {
	key := h.blobKey(c, dgst)
	info, exists, _ := c.Blob.Stat(key)
	if !exists {
		ociError(w, "BLOB_UNKNOWN", "blob not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", dgst)
	if info.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	}
	if r.Method == http.MethodHead {
		return
	}
	rc, err := c.Blob.Get(key)
	if err != nil {
		ociError(w, "BLOB_UNKNOWN", "blob not found", http.StatusNotFound)
		return
	}
	defer rc.Close()
	io.Copy(w, rc)
}

func (h *Handler) deleteBlob(w http.ResponseWriter, c *format.Context, dgst string) {
	c.Blob.Delete(h.blobKey(c, dgst))
	w.WriteHeader(http.StatusAccepted)
}

// --- blob upload -----------------------------------------------------------

func (h *Handler) initiateUpload(w http.ResponseWriter, r *http.Request, c *format.Context, image string) {
	// Monolithic POST: ?digest=sha256:{hex} with full blob body.
	if dgst := r.URL.Query().Get("digest"); dgst != "" {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			ociError(w, "BLOB_UPLOAD_INVALID", "read failed", http.StatusInternalServerError)
			return
		}
		actual := computeDigest(data)
		if actual != dgst {
			ociError(w, "DIGEST_INVALID", "digest mismatch", http.StatusBadRequest)
			return
		}
		if _, err := c.Blob.Put(h.blobKey(c, dgst), bytes.NewReader(data)); err != nil {
			ociError(w, "BLOB_UPLOAD_INVALID", err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/blobs/%s", c.Repo.Name, image, dgst))
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusCreated)
		return
	}

	// Cross-repo blob mount: ?mount={digest}&from={repo}.
	if mount := r.URL.Query().Get("mount"); mount != "" {
		if from := r.URL.Query().Get("from"); from != "" {
			srcKey := from + "/blobs/" + mount
			if rc, err := c.Blob.Get(srcKey); err == nil {
				defer rc.Close()
				if _, err := c.Blob.Put(h.blobKey(c, mount), rc); err == nil {
					w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/blobs/%s", c.Repo.Name, image, mount))
					w.Header().Set("Docker-Content-Digest", mount)
					w.WriteHeader(http.StatusCreated)
					return
				}
			}
		}
		// Mount failed — fall through to regular upload.
	}

	// Initiate chunked upload.
	uuid := newUUID()
	c.Meta.PutJSON(h.ns(c), "uploads/"+uuid, uploadMeta{ImageName: image})
	loc := fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", c.Repo.Name, image, uuid)
	w.Header().Set("Location", loc)
	w.Header().Set("OCI-Upload-UUID", uuid)
	w.Header().Set("OCI-Chunk-Min-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) patchUpload(w http.ResponseWriter, r *http.Request, c *format.Context, image, uuid string) {
	uploadKey := h.uploadKey(c, uuid)

	// Read any previously accumulated bytes.
	var existing []byte
	if rc, err := c.Blob.Get(uploadKey); err == nil {
		existing, _ = io.ReadAll(rc)
		rc.Close()
	}

	chunk, err := io.ReadAll(r.Body)
	if err != nil {
		ociError(w, "BLOB_UPLOAD_INVALID", "read failed", http.StatusInternalServerError)
		return
	}

	combined := append(existing, chunk...)
	if _, err := c.Blob.Put(uploadKey, bytes.NewReader(combined)); err != nil {
		ociError(w, "BLOB_UPLOAD_INVALID", err.Error(), http.StatusInternalServerError)
		return
	}

	last := len(combined) - 1
	if last < 0 {
		last = 0
	}
	loc := fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", c.Repo.Name, image, uuid)
	w.Header().Set("Location", loc)
	w.Header().Set("Range", fmt.Sprintf("0-%d", last))
	w.Header().Set("OCI-Chunk-Min-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) finalizeUpload(w http.ResponseWriter, r *http.Request, c *format.Context, image, uuid string) {
	dgst := r.URL.Query().Get("digest")
	if dgst == "" {
		ociError(w, "DIGEST_INVALID", "digest query parameter required", http.StatusBadRequest)
		return
	}

	uploadKey := h.uploadKey(c, uuid)
	var existing []byte
	if rc, err := c.Blob.Get(uploadKey); err == nil {
		existing, _ = io.ReadAll(rc)
		rc.Close()
		c.Blob.Delete(uploadKey)
	}

	// Optional final chunk in body.
	finalChunk, _ := io.ReadAll(r.Body)
	data := append(existing, finalChunk...)

	actual := computeDigest(data)
	if actual != dgst {
		ociError(w, "DIGEST_INVALID", "digest mismatch", http.StatusBadRequest)
		return
	}

	if _, err := c.Blob.Put(h.blobKey(c, dgst), bytes.NewReader(data)); err != nil {
		ociError(w, "BLOB_UPLOAD_INVALID", err.Error(), http.StatusInternalServerError)
		return
	}
	c.Meta.Delete(h.ns(c), "uploads/"+uuid)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/blobs/%s", c.Repo.Name, image, dgst))
	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

// --- manifest endpoints ----------------------------------------------------

func (h *Handler) getManifest(w http.ResponseWriter, r *http.Request, c *format.Context, image, ref string) {
	dgst, ok := h.resolveRef(c, image, ref)
	if !ok {
		ociError(w, "MANIFEST_UNKNOWN", "manifest not found", http.StatusNotFound)
		return
	}

	data, err := h.readManifest(c, dgst)
	if err != nil {
		ociError(w, "MANIFEST_UNKNOWN", "manifest not found", http.StatusNotFound)
		return
	}

	var mm manifestMeta
	c.Meta.GetJSON(h.ns(c), "manifests/"+dgst, &mm)
	mt := mm.MediaType
	if mt == "" {
		mt = "application/vnd.oci.image.manifest.v1+json"
	}

	w.Header().Set("Content-Type", mt)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(data)
}

func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, c *format.Context, image, ref string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		ociError(w, "MANIFEST_INVALID", "read failed", http.StatusBadRequest)
		return
	}

	dgst := computeDigest(data)
	mt := r.Header.Get("Content-Type")
	if mt == "" {
		mt = "application/vnd.oci.image.manifest.v1+json"
	}

	if _, err := c.Blob.Put(h.manifestKey(c, dgst), bytes.NewReader(data)); err != nil {
		ociError(w, "MANIFEST_INVALID", err.Error(), http.StatusInternalServerError)
		return
	}
	c.Meta.PutJSON(h.ns(c), "manifests/"+dgst, manifestMeta{MediaType: mt, ImageName: image})

	// If reference is a tag, store tag → digest mapping and push timestamp.
	if !strings.HasPrefix(ref, "sha256:") {
		c.Meta.PutJSON(h.ns(c), "tags/"+image+"/"+ref, dgst)                                                        //nolint:errcheck
		c.Meta.PutJSON(h.ns(c), "tag-times/"+image+"/"+ref, time.Now().UTC().Format(time.RFC3339)) //nolint:errcheck
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/manifests/%s", c.Repo.Name, image, dgst))
	w.Header().Set("Docker-Content-Digest", dgst)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) deleteManifest(w http.ResponseWriter, c *format.Context, image, ref string) {
	dgst, ok := h.resolveRef(c, image, ref)
	if !ok {
		ociError(w, "MANIFEST_UNKNOWN", "manifest not found", http.StatusNotFound)
		return
	}

	c.Blob.Delete(h.manifestKey(c, dgst))
	c.Meta.Delete(h.ns(c), "manifests/"+dgst)

	// Remove any tags pointing to this digest.
	if keys, _ := c.Meta.List(h.ns(c)); keys != nil {
		prefix := "tags/" + image + "/"
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			var d string
			if ok, _ := c.Meta.GetJSON(h.ns(c), k, &d); ok && d == dgst {
				c.Meta.Delete(h.ns(c), k)
			}
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// --- tags ------------------------------------------------------------------

func (h *Handler) listTags(w http.ResponseWriter, c *format.Context, image string) {
	keys, _ := c.Meta.List(h.ns(c))
	prefix := "tags/" + image + "/"
	var tags []string
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			tags = append(tags, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"name": image, "tags": tags})
}

// --- proxy pass-through ----------------------------------------------------

func (h *Handler) proxyPass(w http.ResponseWriter, r *http.Request, c *format.Context, image, op, ref string) {
	if c.Repo.Upstream == "" {
		ociError(w, "UNSUPPORTED", "no upstream configured", http.StatusBadGateway)
		return
	}
	// Reconstruct the upstream /v2/ path.
	var upPath string
	switch op {
	case "manifests":
		upPath = "/" + image + "/manifests/" + ref
	case "blobs":
		upPath = "/" + image + "/blobs/" + ref
	case "tags/list":
		upPath = "/" + image + "/tags/list"
	default:
		ociError(w, "UNSUPPORTED", "unsupported proxy operation", http.StatusNotFound)
		return
	}
	upURL := strings.TrimRight(c.Repo.Upstream, "/") + "/v2" + upPath
	req, err := http.NewRequest(r.Method, upURL, nil)
	if err != nil {
		ociError(w, "UNSUPPORTED", err.Error(), http.StatusBadGateway)
		return
	}
	// Forward Accept header (important for manifests).
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.HTTP.Do(req) // #nosec G704 -- upURL is built from admin-configured upstream, not user input
	if err != nil {
		ociError(w, "UNSUPPORTED", "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, hdr := range []string{"Content-Type", "Docker-Content-Digest", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// --- helpers ---------------------------------------------------------------

func (h *Handler) ns(c *format.Context) string { return c.Repo.Name + ":oci" }

func (h *Handler) blobKey(c *format.Context, dgst string) string {
	return c.Repo.Name + "/blobs/" + dgst
}

func (h *Handler) manifestKey(c *format.Context, dgst string) string {
	return c.Repo.Name + "/manifests/" + dgst
}

func (h *Handler) uploadKey(c *format.Context, uuid string) string {
	return c.Repo.Name + "/uploads/" + uuid
}

// resolveRef converts a tag or digest reference to a digest.
func (h *Handler) resolveRef(c *format.Context, image, ref string) (string, bool) {
	if strings.HasPrefix(ref, "sha256:") {
		_, exists, _ := c.Blob.Stat(h.manifestKey(c, ref))
		return ref, exists
	}
	var dgst string
	ok, _ := c.Meta.GetJSON(h.ns(c), "tags/"+image+"/"+ref, &dgst)
	return dgst, ok
}

func (h *Handler) readManifest(c *format.Context, dgst string) ([]byte, error) {
	rc, err := c.Blob.Get(h.manifestKey(c, dgst))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// digest is a no-op helper that documents intent: the ref is already a digest.
func digest(ref string) string { return ref }

// ociError writes an OCI Distribution Spec JSON error response.
func ociError(w http.ResponseWriter, code, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"code": code, "message": message}},
	})
}

// parseOCISub splits the sub-path (after the repo name) into image name,
// operation, and reference. Examples:
//
//	"myapp/manifests/latest"          → image="myapp", op="manifests",      ref="latest"
//	"myapp/blobs/sha256:abc"          → image="myapp", op="blobs",          ref="sha256:abc"
//	"myapp/blobs/uploads/"            → image="myapp", op="blobs/uploads",  ref=""
//	"myapp/blobs/uploads/uuid"        → image="myapp", op="blobs/uploads",  ref="uuid"
//	"org/image/tags/list"             → image="org/image", op="tags/list",  ref=""
func parseOCISub(sub string) (image, op, ref string, ok bool) {
	if idx := strings.Index(sub, "/manifests/"); idx >= 0 {
		return sub[:idx], "manifests", sub[idx+len("/manifests/"):], true
	}
	if idx := strings.Index(sub, "/blobs/uploads/"); idx >= 0 {
		return sub[:idx], "blobs/uploads", sub[idx+len("/blobs/uploads/"):], true
	}
	if strings.HasSuffix(sub, "/blobs/uploads") {
		return strings.TrimSuffix(sub, "/blobs/uploads"), "blobs/uploads", "", true
	}
	if idx := strings.Index(sub, "/blobs/"); idx >= 0 {
		return sub[:idx], "blobs", sub[idx+len("/blobs/"):], true
	}
	if strings.HasSuffix(sub, "/tags/list") {
		return strings.TrimSuffix(sub, "/tags/list"), "tags/list", "", true
	}
	return "", "", "", false
}

func computeDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// BrowseRepo implements format.Browsable.
// OCI tags are stored at meta key "tags/{image}/{tag}".
func (h *Handler) BrowseRepo(c *format.Context) ([]format.BrowseEntry, error) {
	keys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return nil, err
	}
	byImage := map[string][]string{}
	byImageTime := map[string]time.Time{}
	for _, k := range keys {
		if strings.HasPrefix(k, "tags/") {
			rest := strings.TrimPrefix(k, "tags/")
			image, tag, ok := strings.Cut(rest, "/")
			if ok {
				byImage[image] = append(byImage[image], tag)
			}
		} else if strings.HasPrefix(k, "tag-times/") {
			rest := strings.TrimPrefix(k, "tag-times/")
			image, _, ok := strings.Cut(rest, "/")
			if ok {
				var ts string
				if ok2, _ := c.Meta.GetJSON(h.ns(c), k, &ts); ok2 {
					if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(byImageTime[image]) {
						byImageTime[image] = t
					}
				}
			}
		}
	}
	entries := make([]format.BrowseEntry, 0, len(byImage))
	for image, tags := range byImage {
		sort.Strings(tags)
		entries = append(entries, format.BrowseEntry{Name: image, Versions: tags, UpdatedAt: byImageTime[image]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}
