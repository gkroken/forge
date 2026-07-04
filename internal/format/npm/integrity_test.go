package npm

import (
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/proxy"
	"forge/internal/repo"
)

func integCtx(t *testing.T, kind repo.Kind) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return &format.Context{
		Repo: repo.Repository{Name: "npm-repo", Format: "npm", Kind: kind},
		Blob: b, Meta: m,
	}
}

// seedVersion writes the trio a real publish produces: tarball blob,
// per-version record (with dist.shasum), and packument entry.
func seedVersion(t *testing.T, c *format.Context, h *Handler, pkg, ver, body string) {
	t.Helper()
	tarName := lastPathSeg(pkg) + "-" + ver + ".tgz"
	if _, err := c.Blob.Put(c.Key(pkg+"/-/"+tarName), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	vobj := map[string]any{
		"name": pkg, "version": ver,
		"dist": map[string]any{"shasum": blob.SHA1([]byte(body)), "tarball": "http://x/" + tarName},
	}
	if err := c.Meta.PutJSON(h.versNS(c), pkg+":"+ver, vobj); err != nil {
		t.Fatal(err)
	}
	h.regenPackument(c, pkg)
}

func findingKinds(res integrity.Result) map[string]int {
	m := map[string]int{}
	for _, f := range res.Findings {
		m[f.Kind]++
	}
	return m
}

func TestVerifyIntegrity_CleanAndScoped(t *testing.T) {
	c := integCtx(t, repo.Hosted)
	h := New()
	seedVersion(t, c, h, "is-odd", "1.0.0", "tar-bytes")
	seedVersion(t, c, h, "@acme/lib", "2.0.0", "scoped-bytes")
	c.Meta.PutJSON(h.tagsNS(c), "is-odd", map[string]any{"latest": "1.0.0"}) //nolint:errcheck

	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean, got %+v", res.Findings)
	}
	if res.BlobsChecked != 2 || res.BytesRead == 0 {
		t.Errorf("stats wrong: %+v", res)
	}
}

func TestVerifyIntegrity_MissingOrphanMismatch(t *testing.T) {
	c := integCtx(t, repo.Hosted)
	h := New()
	seedVersion(t, c, h, "is-odd", "1.0.0", "tar-bytes")
	seedVersion(t, c, h, "is-odd", "2.0.0", "gone-bytes")
	seedVersion(t, c, h, "is-odd", "3.0.0", "will-flip")

	// missing: record for 2.0.0 loses its tarball.
	c.Blob.Delete(c.Key("is-odd/-/is-odd-2.0.0.tgz")) //nolint:errcheck
	// orphan: tarball with no record anywhere.
	c.Blob.Put(c.Key("is-odd/-/is-odd-9.9.9.tgz"), strings.NewReader("stray")) //nolint:errcheck
	// mismatch: 3.0.0 bytes flipped after publish.
	c.Blob.Put(c.Key("is-odd/-/is-odd-3.0.0.tgz"), strings.NewReader("FLIPPED")) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeFull)
	k := findingKinds(res)
	if k[integrity.KindMissing] != 1 || k[integrity.KindOrphan] != 1 || k[integrity.KindMismatch] != 1 {
		t.Fatalf("kinds = %v, findings %+v", k, res.Findings)
	}
	// Quick mode: no hashing → mismatch invisible, structure still checked.
	qres, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	qk := findingKinds(qres)
	if qk[integrity.KindMismatch] != 0 || qk[integrity.KindMissing] != 1 || qres.BytesRead != 0 {
		t.Errorf("quick mode wrong: %v (bytes=%d)", qk, qres.BytesRead)
	}
}

func TestVerifyIntegrity_Drift(t *testing.T) {
	c := integCtx(t, repo.Hosted)
	h := New()
	seedVersion(t, c, h, "is-odd", "1.0.0", "tar-bytes")

	// Stale packument: claims a version with no record.
	var pack map[string]any
	c.Meta.GetJSON(h.ns(c), "is-odd", &pack) //nolint:errcheck
	pack["versions"].(map[string]any)["9.0.0"] = map[string]any{"version": "9.0.0"}
	c.Meta.PutJSON(h.ns(c), "is-odd", pack) //nolint:errcheck

	// Dist-tag pointing nowhere.
	c.Meta.PutJSON(h.tagsNS(c), "is-odd", map[string]any{"latest": "1.0.0", "beta": "8.0.0"}) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	k := findingKinds(res)
	// 9.0.0 in packument w/o record + beta tag → 2 drift findings; the stray
	// packument version also has no tarball but tarball checks key off records,
	// so no missing/orphan noise.
	if k[integrity.KindDrift] != 2 || len(res.Findings) != 2 {
		t.Fatalf("expected exactly 2 drift, got %+v", res.Findings)
	}

	// Packument deleted entirely → drift.
	c.Meta.Delete(h.ns(c), "is-odd") //nolint:errcheck
	res, _ = h.VerifyIntegrity(c, integrity.ModeQuick)
	found := false
	for _, f := range res.Findings {
		if f.Kind == integrity.KindDrift && strings.Contains(f.Detail, "packument is missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected missing-packument drift, got %+v", res.Findings)
	}
}

func TestVerifyIntegrity_OldFormatPackumentOnly(t *testing.T) {
	// Pre-per-version-record package: packument only. Its tarball must not be
	// flagged orphan and no drift may fire.
	c := integCtx(t, repo.Hosted)
	h := New()
	c.Blob.Put(c.Key("legacy/-/legacy-1.0.0.tgz"), strings.NewReader("old")) //nolint:errcheck
	c.Meta.PutJSON(h.ns(c), "legacy", map[string]any{
		"name":     "legacy",
		"versions": map[string]any{"1.0.0": map[string]any{"version": "1.0.0"}},
	}) //nolint:errcheck

	res, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean for old-format package, got %+v", res.Findings)
	}
}

func TestVerifyIntegrity_ProxyPackumentEntries(t *testing.T) {
	c := integCtx(t, repo.Proxy)
	h := New()
	// Cached tarball with blob-keyed entry: clean.
	c.Blob.Put(c.Key("is-odd/-/is-odd-1.0.0.tgz"), strings.NewReader("tar"))             //nolint:errcheck
	c.Meta.PutJSON(h.proxyNS(c), c.Key("is-odd/-/is-odd-1.0.0.tgz"), proxy.CacheEntry{}) //nolint:errcheck
	// Packument entry (pkg-keyed) whose document exists: clean.
	c.Meta.PutJSON(h.proxyNS(c), "is-odd", proxy.CacheEntry{})          //nolint:errcheck
	c.Meta.PutJSON(h.ns(c), "is-odd", map[string]any{"name": "is-odd"}) //nolint:errcheck
	// Packument entry whose document is gone: missing.
	c.Meta.PutJSON(h.proxyNS(c), "lodash", proxy.CacheEntry{}) //nolint:errcheck

	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	k := findingKinds(res)
	if k[integrity.KindMissing] != 1 || len(res.Findings) != 1 {
		t.Fatalf("expected exactly 1 missing, got %+v", res.Findings)
	}
}

func TestReindex_RebuildsDriftedPackument(t *testing.T) {
	c := integCtx(t, repo.Hosted)
	h := New()
	seedVersion(t, c, h, "is-odd", "1.0.0", "tar-bytes")
	// Corrupt the packument.
	c.Meta.PutJSON(h.ns(c), "is-odd", map[string]any{"name": "is-odd", "versions": map[string]any{}}) //nolint:errcheck

	n, err := h.Reindex(t.Context(), c)
	if err != nil || n != 1 {
		t.Fatalf("Reindex = %d, %v", n, err)
	}
	res, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	if len(res.Findings) != 0 {
		t.Fatalf("expected drift repaired, got %+v", res.Findings)
	}
}
