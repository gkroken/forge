package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/repo"
)

// Compile-time assertion that the handler implements the integrity seam.

// staleUploadAge is how old an in-progress upload buffer must be before it is
// reported as an orphan — younger buffers may belong to a push in flight.
const staleUploadAge = 24 * time.Hour

// VerifyIntegrity implements format.Handler.
//
// OCI is content-addressed, which makes it the most verifiable format: every
// blob and manifest key embeds the sha256 the bytes must hash to, and every
// manifest declares the digests it depends on. The invariants:
//
//   - manifest reference (config, layer, sub-manifest) that is gone → missing
//   - tag or manifestMeta record pointing at a gone manifest        → missing
//   - blob no manifest references / manifest blob with no record    → orphan
//   - upload buffer older than 24h / dangling upload record         → orphan
//   - full mode: blob bytes ≠ the sha256 in its own key             → mismatch
//
// Manifests are small JSON documents and are read (and digest-checked) in
// both modes; quick mode skips hashing the layer blobs only. Proxy OCI repos
// are pure pass-through and store nothing, so there is nothing to verify.
func (h *Handler) VerifyIntegrity(c *format.Context, mode integrity.Mode) (integrity.Result, error) {
	var res integrity.Result
	if c.Repo.Kind != repo.Hosted {
		return res, nil
	}

	keys, err := c.Blob.List(c.Repo.Name + "/")
	if err != nil {
		return res, err
	}
	res.BlobsChecked = len(keys)

	blobDigests := map[string]bool{}     // "sha256:…" present under blobs/
	manifestDigests := map[string]bool{} // "sha256:…" present under manifests/
	blobsPrefix := c.Repo.Name + "/blobs/"
	manifestsPrefix := c.Repo.Name + "/manifests/"
	uploadsPrefix := c.Repo.Name + "/uploads/"
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, blobsPrefix):
			blobDigests[strings.TrimPrefix(k, blobsPrefix)] = true
		case strings.HasPrefix(k, manifestsPrefix):
			manifestDigests[strings.TrimPrefix(k, manifestsPrefix)] = true
		case strings.HasPrefix(k, uploadsPrefix):
			if info, ok, _ := c.Blob.Stat(k); ok && !info.ModTime.IsZero() && time.Since(info.ModTime) > staleUploadAge {
				res.Add(integrity.KindOrphan, k, "", "",
					"in-progress upload buffer abandoned for more than 24h — the push never completed")
			}
		}
	}

	// Manifests: verify their own digest key, parse their references.
	referenced := map[string]bool{} // blob digests referenced by any manifest
	for dgst := range manifestDigests {
		data, err := h.readManifest(c, dgst)
		if err != nil {
			res.Add(integrity.KindMismatch, manifestsPrefix+dgst, "", "", "manifest unreadable: "+err.Error())
			continue
		}
		res.BytesRead += int64(len(data))
		sum := sha256.Sum256(data)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != dgst {
			res.Add(integrity.KindMismatch, manifestsPrefix+dgst, imageOf(c, dgst), "",
				"manifest bytes hash to "+got+" but are stored under "+dgst+" — content-address broken")
		}
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
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if d := m.Config.Digest; d != "" {
			referenced[d] = true
			if !blobDigests[d] {
				res.Add(integrity.KindMissing, blobsPrefix+d, imageOf(c, dgst), "",
					"config blob referenced by manifest "+dgst+" is gone — pulls will fail")
			}
		}
		for _, l := range m.Layers {
			referenced[l.Digest] = true
			if !blobDigests[l.Digest] {
				res.Add(integrity.KindMissing, blobsPrefix+l.Digest, imageOf(c, dgst), "",
					"layer blob referenced by manifest "+dgst+" is gone — pulls will fail")
			}
		}
		for _, sm := range m.Manifests {
			if sm.Digest != "" && !manifestDigests[sm.Digest] {
				res.Add(integrity.KindMissing, manifestsPrefix+sm.Digest, imageOf(c, dgst), "",
					"sub-manifest referenced by index "+dgst+" is gone — pulls of that platform will fail")
			}
		}
	}

	// Meta records: tags, manifest records, upload records.
	metaKeys, err := c.Meta.List(h.ns(c))
	if err != nil {
		return res, err
	}
	recordedManifests := map[string]bool{}
	for _, mk := range metaKeys {
		switch {
		case strings.HasPrefix(mk, "tags/"):
			res.MetaChecked++
			var dgst string
			if ok, _ := c.Meta.GetJSON(h.ns(c), mk, &dgst); !ok {
				continue
			}
			if !manifestDigests[dgst] {
				rest := strings.TrimPrefix(mk, "tags/")
				image, tag := rest, ""
				if i := strings.LastIndex(rest, "/"); i >= 0 {
					image, tag = rest[:i], rest[i+1:]
				}
				res.Add(integrity.KindMissing, manifestsPrefix+dgst, image, tag,
					"tag points at manifest "+dgst+" which is gone — pulls by this tag will fail")
			}
		case strings.HasPrefix(mk, "manifests/"):
			res.MetaChecked++
			dgst := strings.TrimPrefix(mk, "manifests/")
			recordedManifests[dgst] = true
			if !manifestDigests[dgst] {
				res.Add(integrity.KindMissing, manifestsPrefix+dgst, imageOf(c, dgst), "",
					"manifest record exists but the manifest blob is gone")
			}
		case strings.HasPrefix(mk, "uploads/"):
			res.MetaChecked++
			uuid := strings.TrimPrefix(mk, "uploads/")
			if _, ok, _ := c.Blob.Stat(h.uploadKey(c, uuid)); !ok {
				res.Add(integrity.KindOrphan, h.ns(c)+" · "+mk, "", "",
					"upload record has no buffer blob — the upload was abandoned or half-cleaned")
			}
		}
	}
	for dgst := range manifestDigests {
		if !recordedManifests[dgst] {
			res.Add(integrity.KindOrphan, manifestsPrefix+dgst, "", "",
				"manifest blob has no manifestMeta record — its media type is unknown to the registry API")
		}
	}

	// Unreferenced blobs and, in full mode, content-address verification.
	for dgst := range blobDigests {
		key := blobsPrefix + dgst
		if !referenced[dgst] {
			res.Add(integrity.KindOrphan, key, "", "",
				"blob is referenced by no manifest — unreachable except by direct digest pull")
		}
		if mode != integrity.ModeFull {
			continue
		}
		hs, err := integrity.HashBlob(c.Blob, key)
		if err != nil {
			res.Add(integrity.KindMismatch, key, "", "", "blob unreadable: "+err.Error())
			continue
		}
		res.BytesRead += hs.Size
		if got := "sha256:" + hs.SHA256; got != dgst {
			res.Add(integrity.KindMismatch, key, "", "",
				"blob bytes hash to "+got+" but are stored under "+dgst+" — content-address broken")
		}
	}
	return res, nil
}

// imageOf resolves the image name a manifest belongs to via its meta record,
// best-effort.
func imageOf(c *format.Context, dgst string) string {
	var mm manifestMeta
	c.Meta.GetJSON(c.Repo.Name+":oci", "manifests/"+dgst, &mm) //nolint:errcheck
	return mm.ImageName
}
