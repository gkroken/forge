package helm

import (
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/repo"
)

func integCtx(t *testing.T) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	return &format.Context{
		Repo: repo.Repository{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

func seedChart(t *testing.T, c *format.Context, h *Handler, name, ver, body string) {
	t.Helper()
	filename := name + "-" + ver + ".tgz"
	if _, err := c.Blob.Put(c.Key(filename), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	rec := chartRecord{Name: name, Version: ver, Digest: blob.SHA256([]byte(body)), Filename: filename}
	if err := c.Meta.PutJSON(h.ns(c), name+"-"+ver, rec); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyIntegrity_Helm(t *testing.T) {
	c := integCtx(t)
	h := New()
	seedChart(t, c, h, "nginx", "1.0.0", "chart-bytes")
	seedChart(t, c, h, "redis", "2.0.0", "redis-bytes")
	seedChart(t, c, h, "kafka", "3.0.0", "kafka-bytes")

	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected clean, got %+v", res.Findings)
	}

	// missing: record whose blob is gone.
	c.Blob.Delete(c.Key("redis-2.0.0.tgz")) //nolint:errcheck
	// mismatch: chart bytes flipped after upload.
	c.Blob.Put(c.Key("kafka-3.0.0.tgz"), strings.NewReader("FLIPPED")) //nolint:errcheck
	// orphan: chart blob with no record.
	c.Blob.Put(c.Key("stray-9.0.0.tgz"), strings.NewReader("stray")) //nolint:errcheck

	res, _ = h.VerifyIntegrity(c, integrity.ModeFull)
	got := map[string]string{}
	for _, f := range res.Findings {
		got[f.Kind] = f.Component + "@" + f.Version
	}
	if len(res.Findings) != 3 {
		t.Fatalf("expected 3 findings, got %+v", res.Findings)
	}
	if got[integrity.KindMissing] != "redis@2.0.0" || got[integrity.KindMismatch] != "kafka@3.0.0" || got[integrity.KindOrphan] != "stray@9.0.0" {
		t.Errorf("attribution wrong: %v", got)
	}

	// Quick mode: mismatch invisible, missing+orphan still found.
	qres, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	if len(qres.Findings) != 2 || qres.BytesRead != 0 {
		t.Errorf("quick mode wrong: %+v", qres.Findings)
	}
}
