package cran

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/cranplatform"
	"forge/internal/format"
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
