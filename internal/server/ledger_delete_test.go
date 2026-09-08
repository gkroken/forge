package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/helm"
	"forge/internal/format/npm"
	"forge/internal/ledger"
	"forge/internal/meta"
	"forge/internal/repo"
)

// A version removed through its format's own API must not leave a publish-ledger
// row behind. Rows no longer break correctness — a re-publish re-stamps the date
// — but they are read on every retention run, so leaking them makes cleanup
// quietly slower forever, which is the kind of degradation nobody notices.
//
// This drives the real routes rather than calling ledger.Forget directly: the
// property under test is that the handlers call it at all.
func TestNativeDelete_ForgetsLedgerEntry(t *testing.T) {
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "npm-hosted", Format: "npm", Kind: repo.Hosted, Enabled: true, AnonymousRead: true},
		{Name: "helm-hosted", Format: "helm", Kind: repo.Hosted, Enabled: true, AnonymousRead: true},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	reg := format.NewRegistry()
	reg.Register(npm.New())
	reg.Register(helm.New())
	srv := New(mgr, reg, b, m, nil)

	do := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		return rec.Code
	}

	// npm: publish then unpublish one version.
	pkg := `{"name":"p","versions":{"1.0.0":{"name":"p","version":"1.0.0",` +
		`"dist":{"tarball":"http://x/p-1.0.0.tgz"}}},` +
		`"_attachments":{"p-1.0.0.tgz":{"data":"SGVsbG8="}}}`
	if code := do(http.MethodPut, "/repository/npm-hosted/p", pkg); code != http.StatusCreated {
		t.Fatalf("npm publish = %d", code)
	}
	if _, ok := ledger.Load(m, "npm-hosted")[ledger.Key("p", "1.0.0")]; !ok {
		t.Fatal("npm publish did not record a ledger entry")
	}
	do(http.MethodDelete, "/repository/npm-hosted/p/-/p-1.0.0.tgz", "")
	if _, ok := ledger.Load(m, "npm-hosted")[ledger.Key("p", "1.0.0")]; ok {
		t.Error("npm tarball delete left a ledger row behind")
	}
}
