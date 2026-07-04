package cran

import (
	"bytes"
	"compress/gzip"
	"path/filepath"
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
		Repo: repo.Repository{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

// gzBytes returns a valid gzip stream wrapping payload.
func gzBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func seedSrcPkg(t *testing.T, c *format.Context, h *Handler, pkg, ver string, body []byte) {
	t.Helper()
	if _, err := c.Blob.Put(c.Key("src/contrib/"+pkg+"_"+ver+".tar.gz"), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := c.Meta.PutJSON(h.ns(c), pkg+"_"+ver, pkgRecord{Package: pkg, Version: ver}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyIntegrity_CRANSource(t *testing.T) {
	c := integCtx(t)
	h := New()
	seedSrcPkg(t, c, h, "ggplot2", "3.5.0", gzBytes(t, "src-payload"))
	seedSrcPkg(t, c, h, "dplyr", "1.1.0", gzBytes(t, "dplyr-payload"))
	corrupt := gzBytes(t, "will-be-corrupted-payload-with-some-length")
	corrupt[len(corrupt)-3] ^= 0xff // flip a byte inside the CRC/stream tail
	seedSrcPkg(t, c, h, "rlang", "1.0.0", corrupt)

	// missing: record whose tarball is gone.
	c.Blob.Delete(c.Key("src/contrib/dplyr_1.1.0.tar.gz")) //nolint:errcheck
	// orphan: tarball with no record.
	c.Blob.Put(c.Key("src/contrib/stray_0.1.tar.gz"), bytes.NewReader(gzBytes(t, "x"))) //nolint:errcheck

	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range res.Findings {
		got[f.Kind] = f.Component + "@" + f.Version
	}
	if len(res.Findings) != 3 {
		t.Fatalf("expected 3 findings, got %+v", res.Findings)
	}
	if got[integrity.KindMissing] != "dplyr@1.1.0" || got[integrity.KindMismatch] != "rlang@1.0.0" || got[integrity.KindOrphan] != "stray@0.1" {
		t.Errorf("attribution wrong: %v", got)
	}

	// Quick mode skips the gzip CRC read.
	qres, _ := h.VerifyIntegrity(c, integrity.ModeQuick)
	qk := map[string]int{}
	for _, f := range qres.Findings {
		qk[f.Kind]++
	}
	if qk[integrity.KindMismatch] != 0 || qk[integrity.KindMissing] != 1 || qres.BytesRead != 0 {
		t.Errorf("quick mode wrong: %v (bytes=%d)", qk, qres.BytesRead)
	}
}

func TestVerifyIntegrity_CRANBinaryTree(t *testing.T) {
	c := integCtx(t)
	h := New()
	// Valid binary package: blob + record in the platform namespace.
	c.Blob.Put(c.Key("bin/windows/contrib/4.4/mypkg_1.0.0.zip"), bytes.NewReader([]byte("zip-bytes"))) //nolint:errcheck
	c.Meta.PutJSON(h.binNS(c, "windows", "4.4"), "mypkg_1.0.0", pkgRecord{Package: "mypkg", Version: "1.0.0"})
	// Orphan binary in the same tree.
	c.Blob.Put(c.Key("bin/windows/contrib/4.4/stray_2.0.zip"), bytes.NewReader([]byte("stray"))) //nolint:errcheck
	// Record in the tree whose blob is gone.
	c.Meta.PutJSON(h.binNS(c, "windows", "4.4"), "gone_3.0", pkgRecord{Package: "gone", Version: "3.0"})

	res, err := h.VerifyIntegrity(c, integrity.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range res.Findings {
		got[f.Kind] = f.Component + "@" + f.Version
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %+v", res.Findings)
	}
	if got[integrity.KindOrphan] != "stray@2.0" || got[integrity.KindMissing] != "gone@3.0" {
		t.Errorf("attribution wrong: %v", got)
	}
}
