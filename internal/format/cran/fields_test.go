package cran

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

const linkedDesc = "Package: linked\nVersion: 1.0\nTitle: T\nDescription: D.\nPriority: optional\n" +
	"Depends: R (>= 4.0.0)\nImports: utils\nLinkingTo: cpp11 (>= 0.5.2),\n    BH\nSuggests: testthat\n" +
	"Enhances: other\nLicense: MIT\nLicense_is_FOSS: yes\nNeedsCompilation: yes\n"

// forge's generated index carries R's standard repository fields. Without
// LinkingTo, install.packages through a group never installed a package's
// LinkingTo dependencies and its compile failed ("dependency 'cpp11' is not
// available for package 'tzdb'", reproduced with R 4.6.1).
func TestIndex_CarriesStandardFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/src/contrib/PACKAGES" {
			io.WriteString(w, "Package: tzdb\nVersion: 0.5.0\nDepends: R (>= 4.0.0)\nLinkingTo: cpp11 (>= 0.5.2)\n"+ //nolint:errcheck
				"Suggests: covr, testthat (>= 3.0.0)\nLicense: MIT + file LICENSE\nNeedsCompilation: yes\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy, Upstream: upstream.URL},
		{Name: "cran-public", Format: "cran", Kind: repo.Group, Members: []string{"cran-hosted", "cran-proxy"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	ctx := func(name string) *format.Context {
		r, _ := mgr.Get(name)
		return &format.Context{Repo: r, Blob: b, Meta: m, Repos: mgr, HTTP: upstream.Client()}
	}
	tgz := lbTarball(t, []tarEntry{{name: "linked/DESCRIPTION", body: []byte(linkedDesc)}})
	if rw := cranServe(ctx("cran-hosted"), http.MethodPut, "src/contrib/linked_1.0.tar.gz", bytes.NewReader(tgz)); rw.Code != http.StatusCreated {
		t.Fatalf("publish: %d %s", rw.Code, rw.Body)
	}

	idx := cranServe(ctx("cran-public"), http.MethodGet, "src/contrib/PACKAGES", nil).Body.String()
	for _, want := range []string{
		"Package: linked\nVersion: 1.0\nPriority: optional\nDepends: R (>= 4.0.0)\nImports: utils\n" +
			"LinkingTo: cpp11 (>= 0.5.2), BH\nSuggests: testthat\nEnhances: other\nLicense: MIT\n" +
			"License_is_FOSS: yes\nNeedsCompilation: yes\n",
		"Package: tzdb\nVersion: 0.5.0\nDepends: R (>= 4.0.0)\nLinkingTo: cpp11 (>= 0.5.2)\n" +
			"Suggests: covr, testthat (>= 3.0.0)\nLicense: MIT + file LICENSE\nNeedsCompilation: yes\n",
	} {
		if !strings.Contains(idx, want) {
			t.Errorf("group PACKAGES lacks\n%s\ngot:\n%s", want, idx)
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(cranServe(ctx("cran-public"), http.MethodGet, "src/contrib/PACKAGES.rds", nil).Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	rds, _ := io.ReadAll(zr)
	for _, want := range []string{"LinkingTo", "cpp11 (>= 0.5.2), BH", "cpp11 (>= 0.5.2)"} {
		if !bytes.Contains(rds, []byte(want)) {
			t.Errorf("group PACKAGES.rds lacks %q", want)
		}
	}
}

// A record stored before the index carried these fields is re-read from its
// tarball once, keeping what DESCRIPTION does not say.
func TestIndex_UpgradesOldRecords(t *testing.T) {
	c := newCRANCtx(t)
	tgz := lbTarball(t, []tarEntry{{name: "linked/DESCRIPTION", body: []byte(linkedDesc)}})
	c.Blob.Put(c.Key("src/contrib/linked_1.0.tar.gz"), bytes.NewReader(tgz)) //nolint:errcheck
	published := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	c.Meta.PutJSON(New().ns(c), "linked_1.0", pkgRecord{Package: "linked", Version: "1.0", Imports: "utils", UploadedAt: published}) //nolint:errcheck

	if idx := cranServe(c, http.MethodGet, "src/contrib/PACKAGES", nil).Body.String(); !strings.Contains(idx, "LinkingTo: cpp11 (>= 0.5.2), BH\n") {
		t.Errorf("old record not upgraded:\n%s", idx)
	}
	var rec pkgRecord
	c.Meta.GetJSON(New().ns(c), "linked_1.0", &rec) //nolint:errcheck
	if rec.Schema != recordSchema || rec.LinkingTo == "" || !rec.UploadedAt.Equal(published) {
		t.Errorf("stored record: schema %d linkingTo %q uploadedAt %v", rec.Schema, rec.LinkingTo, rec.UploadedAt)
	}
	// A record whose tarball is gone is served as it was.
	c.Meta.PutJSON(New().ns(c), "gone_1.0", pkgRecord{Package: "gone", Version: "1.0"}) //nolint:errcheck
	if idx := cranServe(c, http.MethodGet, "src/contrib/PACKAGES", nil).Body.String(); !strings.Contains(idx, "Package: gone\nVersion: 1.0\n") {
		t.Errorf("record without a tarball dropped:\n%s", idx)
	}
}
