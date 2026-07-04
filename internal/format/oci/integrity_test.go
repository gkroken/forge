package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/repo"
)

// seedImage pushes a minimal image (config blob + one layer + manifest +
// tag) directly through the storage layout and returns the digests.
func seedImage(t *testing.T, c *format.Context, h *Handler, image, tag string) (cfgD, layerD, manD string) {
	t.Helper()
	cfg := []byte(`{"os":"linux"}`)
	layer := []byte("layer-bytes-" + image)
	cfgD, layerD = computeDigest(cfg), computeDigest(layer)
	man, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": cfgD, "size": len(cfg)},
		"layers":        []map[string]any{{"digest": layerD, "size": len(layer)}},
	})
	manD = computeDigest(man)
	for k, data := range map[string][]byte{
		h.blobKey(c, cfgD):     cfg,
		h.blobKey(c, layerD):   layer,
		h.manifestKey(c, manD): man,
	} {
		if _, err := c.Blob.Put(k, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	c.Meta.PutJSON(h.ns(c), "manifests/"+manD, manifestMeta{MediaType: "application/vnd.oci.image.manifest.v1+json", ImageName: image}) //nolint:errcheck
	c.Meta.PutJSON(h.ns(c), "tags/"+image+"/"+tag, manD)                                                                                //nolint:errcheck
	return cfgD, layerD, manD
}

func TestVerifyIntegrity_OCIClean(t *testing.T) {
	c := newCtx(t)
	h := New()
	seedImage(t, c, h, "app", "v1")
	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean, got %+v", res.Findings)
	}
	if res.BlobsChecked != 3 || res.BytesRead == 0 {
		t.Errorf("stats wrong: %+v", res)
	}
}

func TestVerifyIntegrity_OCIMissingAndOrphan(t *testing.T) {
	c := newCtx(t)
	h := New()
	_, layerD, manD := seedImage(t, c, h, "app", "v1")

	// missing layer behind the manifest.
	c.Blob.Delete(h.blobKey(c, layerD)) //nolint:errcheck
	// orphan blob nothing references.
	stray := []byte("stray-bytes")
	c.Blob.Put(h.blobKey(c, computeDigest(stray)), bytes.NewReader(stray)) //nolint:errcheck
	// tag pointing at a manifest that is gone.
	c.Meta.PutJSON(h.ns(c), "tags/app/v2", "sha256:"+fmt.Sprintf("%064d", 0)) //nolint:errcheck
	// manifest blob without a manifestMeta record.
	extraMan := []byte(`{"schemaVersion":2}`)
	c.Blob.Put(h.manifestKey(c, computeDigest(extraMan)), bytes.NewReader(extraMan)) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	k := map[string]int{}
	for _, f := range res.Findings {
		k[f.Kind]++
	}
	// missing: layer (from manifest parse) + v2 tag target.
	// orphan: stray blob + unrecorded manifest blob.
	if k[integrity.KindMissing] != 2 || k[integrity.KindOrphan] != 2 {
		t.Fatalf("kinds = %v, findings %+v", k, res.Findings)
	}
	_ = manD
}

func TestVerifyIntegrity_OCIMismatch(t *testing.T) {
	c := newCtx(t)
	h := New()
	_, layerD, _ := seedImage(t, c, h, "app", "v1")
	// Flip the layer's bytes in place: content no longer matches its address.
	c.Blob.Put(h.blobKey(c, layerD), bytes.NewReader([]byte("FLIPPED"))) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeFull)
	var mismatches int
	for _, f := range res.Findings {
		if f.Kind == integrity.KindMismatch {
			mismatches++
		}
	}
	if mismatches != 1 {
		t.Fatalf("expected 1 mismatch, got %+v", res.Findings)
	}
	// Quick mode does not hash layer blobs.
	qres, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	for _, f := range qres.Findings {
		if f.Kind == integrity.KindMismatch {
			t.Fatalf("quick mode must not detect blob mismatch: %+v", f)
		}
	}
}

func TestVerifyIntegrity_OCIStaleUpload(t *testing.T) {
	// Build the context by hand so the blob FS root is known — the stale
	// buffer's mtime must be aged on disk.
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := &format.Context{
		Repo: repo.Repository{Name: "docker-hosted", Format: "oci", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
	h := New()
	seedImage(t, c, h, "app", "v1")
	// Fresh upload buffer: not flagged.
	c.Blob.Put(h.uploadKey(c, "fresh-uuid"), bytes.NewReader([]byte("partial"))) //nolint:errcheck
	c.Meta.PutJSON(h.ns(c), "uploads/fresh-uuid", uploadMeta{ImageName: "app"})  //nolint:errcheck
	// Stale upload buffer: age it past the threshold on disk.
	c.Blob.Put(h.uploadKey(c, "old-uuid"), bytes.NewReader([]byte("partial"))) //nolint:errcheck
	old := time.Now().Add(-48 * time.Hour)
	buf := filepath.Join(dir, "b", h.uploadKey(c, "old-uuid"))
	if err := os.Chtimes(buf, old, old); err != nil {
		t.Fatal(err)
	}
	// Upload record whose buffer is gone.
	c.Meta.PutJSON(h.ns(c), "uploads/ghost-uuid", uploadMeta{ImageName: "app"}) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	var orphans int
	for _, f := range res.Findings {
		if f.Kind == integrity.KindOrphan {
			orphans++
		}
	}
	if orphans != 2 { // stale buffer + ghost record; fresh buffer not flagged
		t.Fatalf("expected 2 orphans, got %+v", res.Findings)
	}
}
