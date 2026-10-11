package cran

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"forge/internal/blob"
	"forge/internal/cranplatform"
	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/ledger"
	"forge/internal/meta"
	"forge/internal/repo"
)

const lbPath = "bin/linux/noble-x86_64/contrib/4.6/lb_1.0.2.tar.gz"

// linuxCtx is a binary repository wired as the plan describes: hosted,
// noOverwrite (so its store refuses overwrites, as repoBlob makes it), a
// sourceRepo, and a registered key. reads counts source fetches.
func linuxCtx(t *testing.T, reads *int) *format.Context {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	if err := cranplatform.NewStore(m).Put(lbPlatform()); err != nil {
		t.Fatal(err)
	}
	src := lbTarball(t, []tarEntry{{name: "lb/DESCRIPTION", body: []byte(lbSrcDesc)}, {name: "lb/R/x.R", body: []byte("1")}})
	old := lbTarball(t, []tarEntry{{name: "lb/DESCRIPTION", body: []byte(strings.Replace(lbSrcDesc, "1.0.2", "0.9", 1))}})
	return &format.Context{
		Repo: repo.Repository{Name: "cran-linux", Format: "cran", Kind: repo.Hosted, NoOverwrite: true, SourceRepo: "cran-public"},
		Blob: blob.NoOverwrite(b), Meta: m,
		ReadRepo: func(name, sub string) (int, []byte) {
			*reads++
			switch {
			case name != "cran-public":
				return 404, nil
			case sub == "src/contrib/lb_1.0.2.tar.gz":
				return 200, src
			case sub == "src/contrib/Archive/lb/lb_0.9.tar.gz":
				return 200, old
			case strings.Contains(sub, "flaky"):
				return 502, nil
			}
			return 404, nil
		},
	}
}

func linuxPut(c *format.Context, sub, digest string, body []byte) *httptest.ResponseRecorder {
	c.Sub = sub
	r := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(body))
	if digest != "" {
		r.Header.Set(BuilderDigestHeader, digest)
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, r, c)
	return rw
}

func TestLinuxUpload_StoresValidatedBinary(t *testing.T) {
	var reads int
	c := linuxCtx(t, &reads)
	digest := lbPlatform().BuilderDigest
	bin := lbTarball(t, lbEntries(t))

	if rw := linuxPut(c, lbPath, digest, bin); rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	if rw := cranServe(c, http.MethodGet, lbPath, nil); rw.Code != 200 || !bytes.Equal(rw.Body.Bytes(), bin) {
		t.Fatalf("download: %d", rw.Code)
	}
	idx := cranServe(c, http.MethodGet, "bin/linux/noble-x86_64/contrib/4.6/PACKAGES", nil).Body.String()
	if !strings.Contains(idx, "Package: lb") || !strings.Contains(idx, "Built: R 4.6.1; x86_64-pc-linux-gnu") {
		t.Fatalf("index:\n%s", idx)
	}
	if _, ok := ledger.Load(c.Meta, c.Repo.Name)[ledger.Key("lb", "1.0.2")]; !ok {
		t.Error("upload not recorded in the ledger")
	}

	// A second upload of the key is a 409 — before any source is fetched.
	before := reads
	if rw := linuxPut(c, lbPath, digest, bin); rw.Code != http.StatusConflict {
		t.Fatalf("re-upload: %d", rw.Code)
	}
	if reads != before {
		t.Error("a re-upload fetched source before discovering the conflict")
	}
	// Removal is one call, after which the key may be uploaded again.
	if rw := cranServe(c, http.MethodDelete, lbPath, nil); rw.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rw.Code)
	}
	if rw := linuxPut(c, lbPath, digest, bin); rw.Code != http.StatusCreated {
		t.Fatalf("upload after removal: %d %s", rw.Code, rw.Body)
	}

	// An archived version is checked against CRAN's Archive path.
	old := lbTarball(t, set(lbEntries(t), "lb/DESCRIPTION",
		[]byte(strings.Replace(lbSrcDesc, "1.0.2", "0.9", 1)+lbBuilt)))
	if rw := linuxPut(c, "bin/linux/noble-x86_64/contrib/4.6/lb_0.9.tar.gz", digest, old); rw.Code != http.StatusCreated {
		t.Fatalf("archived version: %d %s", rw.Code, rw.Body)
	}
}

func TestLinuxUpload_Refusals(t *testing.T) {
	digest := lbPlatform().BuilderDigest
	cases := []struct {
		name   string
		mod    func(*format.Context)
		sub    string
		digest string
		body   func(t *testing.T) []byte
		code   int
		want   string
	}{
		{name: "unregistered key", sub: "bin/linux/noble-x86_64/contrib/4.5/lb_1.0.2.tar.gz", code: 403, want: "not registered"},
		{name: "no arch in platform", sub: "bin/linux/noble/contrib/4.6/lb_1.0.2.tar.gz", code: 400, want: "not a Linux binary path"},
		{name: "nested platform", sub: "bin/linux/noble-x86_64/x/contrib/4.6/lb_1.0.2.tar.gz", code: 400, want: "not a Linux binary path"},
		{name: ".tgz under linux", sub: "bin/linux/noble-x86_64/contrib/4.6/lb_1.0.2.tgz", code: 404, want: ".tar.gz"},
		{name: "repo allows overwrite", mod: func(c *format.Context) { c.Repo.NoOverwrite = false }, code: 403, want: "noOverwrite"},
		{name: "no sourceRepo", mod: func(c *format.Context) { c.Repo.SourceRepo = "" }, code: 403, want: "sourceRepo"},
		{name: "no digest", digest: "-", code: 400, want: BuilderDigestHeader},
		{name: "other builder", digest: "sha256:" + strings.Repeat("00", 32), code: 403, want: "registered for builder image"},
		{name: "no source", sub: "bin/linux/noble-x86_64/contrib/4.6/lb_2.0.tar.gz", code: 400, want: "serves no source"},
		{name: "source fetch fails", sub: "bin/linux/noble-x86_64/contrib/4.6/flaky_1.0.tar.gz", code: 502, want: "retry"},
		{name: "server cannot read", mod: func(c *format.Context) { c.ReadRepo = nil }, code: 500, want: "unavailable"},
		{name: "invalid binary", code: 400, want: "R 4.6",
			body: func(t *testing.T) []byte {
				return lbTarball(t, set(lbEntries(t), "lb/DESCRIPTION", []byte(lbSrcDesc+strings.Replace(lbBuilt, "4.6.1", "4.5.1", 1))))
			}},
		{name: "proxy repo", mod: func(c *format.Context) { c.Repo.Kind = repo.Proxy }, code: 405, want: "hosted"},
	}
	for _, tc := range cases {
		var reads int
		c := linuxCtx(t, &reads)
		if tc.mod != nil {
			tc.mod(c)
		}
		sub, d := lbPath, digest
		if tc.sub != "" {
			sub = tc.sub
		}
		switch tc.digest {
		case "":
		case "-":
			d = ""
		default:
			d = tc.digest
		}
		body := lbTarball(t, lbEntries(t))
		if tc.body != nil {
			body = tc.body(t)
		}
		rw := linuxPut(c, sub, d, body)
		if rw.Code != tc.code || !strings.Contains(rw.Body.String(), tc.want) {
			t.Errorf("%s: %d %q, want %d mentioning %q", tc.name, rw.Code, strings.TrimSpace(rw.Body.String()), tc.code, tc.want)
		}
		if keys, _ := c.Blob.List(c.Repo.Name + "/"); len(keys) != 0 {
			t.Errorf("%s: refused upload stored %v", tc.name, keys)
		}
	}
}

func TestLinuxUpload_RecordsProvenance(t *testing.T) {
	var reads int
	c := linuxCtx(t, &reads)
	c.Actor = "builder-noble"
	bin := lbTarball(t, lbEntries(t))
	c.Sub = lbPath
	r := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(bin))
	r.Header.Set(BuilderDigestHeader, lbPlatform().BuilderDigest)
	r.Header.Set(LinkingToHeader, "Rcpp 1.0.14, BH 1.87.0-1")
	rw := httptest.NewRecorder()
	New().Serve(rw, r, c)
	if rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	var rec pkgRecord
	c.Meta.GetJSON(New().binNS(c, "linux/noble-x86_64", "4.6"), "lb_1.0.2", &rec) //nolint:errcheck
	p := rec.Linux
	if p == nil {
		t.Fatal("no provenance recorded")
	}
	_, src := c.ReadRepo("cran-public", "src/contrib/lb_1.0.2.tar.gz")
	want := linuxProvenance{
		Key: "noble-x86_64/4.6", Uploader: "builder-noble", BuilderImage: "img", BuilderDigest: lbPlatform().BuilderDigest,
		SourceRepo: "cran-public", SourceSHA256: sha256hex(src), SHA256: sha256hex(bin),
		LinkingTo: "Rcpp 1.0.14, BH 1.87.0-1", Needs: []string{"libc.so.6"}, MaxGlibc: "2.14",
	}
	if got, _ := json.Marshal(p); string(got) != mustJSON(want) {
		t.Errorf("provenance\n got %s\nwant %s", got, mustJSON(want))
	}

	// A LinkingTo header that could not be shown on one line is refused.
	c2 := linuxCtx(t, &reads)
	c2.Sub = lbPath
	r = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(bin))
	r.Header.Set(BuilderDigestHeader, lbPlatform().BuilderDigest)
	r.Header.Set(LinkingToHeader, strings.Repeat("x", 5000))
	rw = httptest.NewRecorder()
	New().Serve(rw, r, c2)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("oversized %s: %d", LinkingToHeader, rw.Code)
	}
}

// A body forge could not read is not a verdict on the binary: a builder
// treats 400 as final, so a broken transfer must be told apart from a bad
// binary (issue: forge-issues/007).
func TestLinuxUpload_UnreadableBody(t *testing.T) {
	digest := lbPlatform().BuilderDigest
	bin := lbTarball(t, lbEntries(t))
	for _, tc := range []struct {
		name string
		body func(w http.ResponseWriter) io.ReadCloser
		code int
	}{
		{name: "reset mid-upload", code: http.StatusRequestTimeout, body: func(http.ResponseWriter) io.ReadCloser {
			return io.NopCloser(io.MultiReader(bytes.NewReader(bin[:10]), iotest.ErrReader(errors.New("connection reset by peer"))))
		}},
		{name: "over the upload limit", code: http.StatusRequestEntityTooLarge, body: func(w http.ResponseWriter) io.ReadCloser {
			return http.MaxBytesReader(w, io.NopCloser(bytes.NewReader(bin)), 10)
		}},
	} {
		var reads int
		c := linuxCtx(t, &reads)
		var reported []string
		c.Reject = func(status int, kind, detail string) { reported = append(reported, detail) }
		c.Sub = lbPath
		rw := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/", nil)
		r.Body = tc.body(rw)
		r.Header.Set(BuilderDigestHeader, digest)
		New().Serve(rw, r, c)
		if rw.Code != tc.code {
			t.Errorf("%s: %d %q, want %d", tc.name, rw.Code, strings.TrimSpace(rw.Body.String()), tc.code)
		}
		if tc.code == http.StatusRequestTimeout && len(reported) != 0 {
			t.Errorf("%s: a broken transfer was reported as a rejected build: %v", tc.name, reported)
		}
		if keys, _ := c.Blob.List(c.Repo.Name + "/"); len(keys) != 0 {
			t.Errorf("%s: stored %v", tc.name, keys)
		}
	}
}

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// A policy refusal is reported (audit + webhook via c.Reject); a lost race
// (409) and upstream trouble (502) are not policy and are not.
func TestLinuxUpload_RejectionsAreReported(t *testing.T) {
	digest := lbPlatform().BuilderDigest
	for _, tc := range []struct {
		name, sub, digest string
		before            func(*format.Context)
		code              int
		reported          bool
	}{
		{name: "wrong builder", sub: lbPath, digest: "sha256:" + strings.Repeat("00", 32), code: 403, reported: true},
		{name: "invalid binary", sub: "bin/linux/noble-x86_64/contrib/4.6/lb_2.0.tar.gz", digest: digest, code: 400, reported: true},
		{name: "lost race", sub: lbPath, digest: digest, code: 409,
			before: func(c *format.Context) { c.Blob.Put(c.Key(lbPath), bytes.NewReader([]byte("x"))) }}, //nolint:errcheck
		{name: "upstream trouble", sub: "bin/linux/noble-x86_64/contrib/4.6/flaky_1.0.tar.gz", digest: digest, code: 502},
	} {
		var reads int
		c := linuxCtx(t, &reads)
		var got []string
		c.Reject = func(status int, kind, detail string) { got = append(got, kind+" "+detail) }
		if tc.before != nil {
			tc.before(c)
		}
		rw := linuxPut(c, tc.sub, tc.digest, lbTarball(t, lbEntries(t)))
		if rw.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.name, rw.Code, tc.code)
		}
		if (len(got) > 0) != tc.reported {
			t.Errorf("%s: reported %v, want reported=%v", tc.name, got, tc.reported)
		}
		if tc.reported && (len(got) != 1 || !strings.HasPrefix(got[0], "cran-linux-binary ") ||
			!strings.Contains(got[0], strings.TrimSpace(rw.Body.String()))) {
			t.Errorf("%s: report %q does not carry the reason %q", tc.name, got, rw.Body.String())
		}
	}
}

// Integrity verification covers the Linux tree: a binary is checked exactly
// against the sha256 recorded at upload, and missing/orphaned binaries are
// reported like any other CRAN artifact.
func TestLinuxIntegrity(t *testing.T) {
	var reads int
	c := linuxCtx(t, &reads)
	bin := lbTarball(t, lbEntries(t))
	if rw := linuxPut(c, lbPath, lbPlatform().BuilderDigest, bin); rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	kinds := func(mode integrity.Mode) []string {
		res, err := New().VerifyIntegrity(c, mode)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range res.Findings {
			out = append(out, string(f.Kind))
		}
		return out
	}
	if got := kinds(integrity.ModeFull); len(got) != 0 {
		t.Fatalf("clean binary repo reported %v", got)
	}

	// Same length, still a valid gzip, different bytes: only the digest sees it.
	tampered := lbTarball(t, set(lbEntries(t), "lb/R/lb", []byte("lazy-load stuB")))
	c.Blob.Delete(c.Key(lbPath))                         //nolint:errcheck
	c.Blob.Put(c.Key(lbPath), bytes.NewReader(tampered)) //nolint:errcheck
	if got := kinds(integrity.ModeFull); strings.Join(got, ",") != string(integrity.KindMismatch) {
		t.Errorf("tampered binary: %v, want one mismatch", got)
	}
	if got := kinds(integrity.ModeQuick); len(got) != 0 {
		t.Errorf("quick mode reads no bytes, yet reported %v", got)
	}

	c.Blob.Delete(c.Key(lbPath)) //nolint:errcheck
	if got := kinds(integrity.ModeQuick); strings.Join(got, ",") != string(integrity.KindMissing) {
		t.Errorf("deleted binary: %v, want one missing", got)
	}
	c.Blob.Put(c.Key("bin/linux/noble-x86_64/contrib/4.6/stray_1.0.tar.gz"), bytes.NewReader(bin)) //nolint:errcheck
	if got := kinds(integrity.ModeQuick); !strings.Contains(strings.Join(got, ","), string(integrity.KindOrphan)) {
		t.Errorf("unrecorded binary: %v, want an orphan", got)
	}
}
