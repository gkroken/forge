package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"forge/internal/repo"
)

// OCI promotion is a local content-addressed copy: it walks the tagged
// manifest (recursing into an image index), copies every referenced blob and
// sub-manifest into the target by digest, then pushes the top manifest under
// the tag so the target's tag pointer is created. Manifest bytes are copied
// verbatim — the digest must survive or the image breaks, which is the point.
// It mirrors the Nexus migrator's OCI strategy but reads from the local blob
// store instead of a remote registry.

// resolveOCIDigest returns the manifest digest a tag points at (or the ref
// itself when it is already a digest).
func (s *Server) resolveOCIDigest(repoName, image, ref string) (string, bool) {
	if len(ref) > 7 && ref[:7] == "sha256:" {
		return ref, true
	}
	var d string
	ok, _ := s.Meta.GetJSON(repoName+":oci", "tags/"+image+"/"+ref, &d)
	return d, ok
}

// ociManifestBytes best-effort sums the manifest, its config and layers (one
// level; index children are summed too) for the target quota pre-check.
func (s *Server) ociManifestBytes(repoName, image, tag string) int64 {
	digest, ok := s.resolveOCIDigest(repoName, image, tag)
	if !ok {
		return 0
	}
	var total int64
	var walk func(d string)
	walk = func(d string) {
		if info, ok, _ := s.Blob.Stat(repoName + "/manifests/" + d); ok {
			total += info.Size
		}
		data, _, err := s.readBlob(repoName + "/manifests/" + d)
		if err != nil {
			return
		}
		var m ociManifest
		if json.Unmarshal(data, &m) != nil {
			return
		}
		for _, sub := range m.Manifests {
			walk(sub.Digest)
		}
		if m.Config.Digest != "" {
			if info, ok, _ := s.Blob.Stat(repoName + "/blobs/" + m.Config.Digest); ok {
				total += info.Size
			}
		}
		for _, l := range m.Layers {
			if info, ok, _ := s.Blob.Stat(repoName + "/blobs/" + l.Digest); ok {
				total += info.Size
			}
		}
	}
	walk(digest)
	return total
}

func (s *Server) promoteOCI(ctx context.Context, src, tgt repo.Repository, image, tag, publicBase string) (string, int64, error) {
	topDigest, ok := s.resolveOCIDigest(src.Name, image, tag)
	if !ok {
		return "", 0, promoteErr(http.StatusNotFound, "%s:%s not found in %s", image, tag, src.Name)
	}
	var copied int64

	copyBlob := func(digest string) error {
		data, _, err := s.readBlob(src.Name + "/blobs/" + digest)
		if err != nil {
			return fmt.Errorf("read blob %s: %w", digest, err)
		}
		rec := s.internalServe(ctx, http.MethodPost, tgt.Name, image+"/blobs/uploads", "digest="+url.QueryEscape(digest), bytes.NewReader(data), nil, publicBase)
		if !rec.ok() {
			return rec.err()
		}
		copied += int64(len(data))
		return nil
	}

	// copyManifest reads the manifest for ref (a digest, or the top tag),
	// copies the blobs/sub-manifests it references, then pushes it under ref.
	var copyManifest func(ref string) error
	copyManifest = func(ref string) error {
		digest, ok := s.resolveOCIDigest(src.Name, image, ref)
		if !ok {
			return fmt.Errorf("resolve manifest %s", ref)
		}
		raw, _, err := s.readBlob(src.Name + "/manifests/" + digest)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", ref, err)
		}
		var m ociManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("parse manifest %s: %w", ref, err)
		}
		for _, sub := range m.Manifests {
			if err := copyManifest(sub.Digest); err != nil {
				return err
			}
		}
		if m.Config.Digest != "" {
			if err := copyBlob(m.Config.Digest); err != nil {
				return err
			}
		}
		for _, l := range m.Layers {
			if err := copyBlob(l.Digest); err != nil {
				return err
			}
		}
		h := http.Header{}
		if m.MediaType != "" {
			h.Set("Content-Type", m.MediaType)
		}
		rec := s.internalServe(ctx, http.MethodPut, tgt.Name, image+"/manifests/"+ref, "", bytes.NewReader(raw), h, publicBase)
		if !rec.ok() {
			return fmt.Errorf("push manifest %s: %w", ref, rec.err())
		}
		copied += int64(len(raw))
		return nil
	}

	if err := copyManifest(tag); err != nil {
		return "", 0, err
	}
	return topDigest, copied, nil
}
