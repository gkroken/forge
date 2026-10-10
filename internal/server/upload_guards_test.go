package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/cleanup"
	"forge/internal/repo"
)

// handleRepo applies two storage guards — the immutable write-once wrapper and
// the quota gate — and for a long time it was the only path that did. The
// browser upload form and the Nexus migration build a format.Context themselves
// and dispatch to a handler in process, so both wrote through neither. An
// "immutable" repository could be overwritten through the upload form, and a
// full repository kept accepting uploads through it.
//
// These pin the guards to the repository rather than to one route.

func uploadChart(t *testing.T, srv *Server, repoName, name, version string) *httptest.ResponseRecorder {
	t.Helper()
	chart := makeChartTGZ(t, name, version)
	body, ct := buildMultipartForm(t, "file", name+"-"+version+".tgz", chart)
	req := httptest.NewRequest(http.MethodPost, "/ui/repos/"+repoName+"/upload", body)
	req.Header.Set("Content-Type", ct)
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, req)
	return rw
}

func TestUIUpload_RespectsImmutability(t *testing.T) {
	srv := newUIServer(t)
	rp, ok := srv.Repos.Get("helm-hosted")
	if !ok {
		t.Fatal("helm-hosted missing")
	}
	rp.Immutable = true
	if err := srv.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}

	if rw := uploadChart(t, srv, "helm-hosted", "locked", "1.0.0"); rw.Code != http.StatusOK {
		t.Fatalf("first upload: %d", rw.Code)
	}
	first, ok, _ := srv.Blob.Stat("helm-hosted/locked-1.0.0.tgz")
	if !ok {
		t.Fatal("first upload not stored")
	}

	// The same coordinate again must be refused, exactly as the protocol path is.
	rw := uploadChart(t, srv, "helm-hosted", "locked", "1.0.0")
	if !strings.Contains(rw.Body.String(), "immutable") {
		t.Errorf("re-upload to an immutable repo was not refused:\n%.300s", rw.Body.String())
	}
	again, _, _ := srv.Blob.Stat("helm-hosted/locked-1.0.0.tgz")
	if again.SHA256 != first.SHA256 {
		t.Error("the stored artifact changed: the upload form bypassed write-once")
	}
}

func TestUIUpload_RespectsQuota(t *testing.T) {
	srv := newUIServer(t)
	rp, ok := srv.Repos.Get("helm-hosted")
	if !ok {
		t.Fatal("helm-hosted missing")
	}
	tiny := 0.0000001 // ~107 bytes
	rp.QuotaGB = &tiny
	if err := srv.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}

	rw := uploadChart(t, srv, "helm-hosted", "toobig", "1.0.0")
	if !strings.Contains(rw.Body.String(), "quota") {
		t.Errorf("upload past the quota was accepted:\n%.300s", rw.Body.String())
	}
	if _, stored, _ := srv.Blob.Stat("helm-hosted/toobig-1.0.0.tgz"); stored {
		t.Error("the artifact was stored despite the quota being exceeded")
	}
}

// repoBlob is the single decision every dispatch path shares.
func TestRepoBlob_WrapsOnlyImmutableRepos(t *testing.T) {
	srv := newAdminServer(t)
	plain := repo.Repository{Name: "plain", Format: "npm", Kind: repo.Hosted}
	locked := repo.Repository{Name: "locked", Format: "npm", Kind: repo.Hosted, Immutable: true}

	if _, err := srv.repoBlob(plain).Put("plain/a", strings.NewReader("one")); err != nil {
		t.Fatalf("plain repo first write: %v", err)
	}
	if _, err := srv.repoBlob(plain).Put("plain/a", strings.NewReader("two")); err != nil {
		t.Errorf("a mutable repo must still allow overwrite: %v", err)
	}
	if _, err := srv.repoBlob(locked).Put("locked/a", strings.NewReader("one")); err != nil {
		t.Fatalf("immutable repo first write: %v", err)
	}
	if _, err := srv.repoBlob(locked).Put("locked/a", strings.NewReader("two")); err == nil {
		t.Error("an immutable repo accepted an overwrite through repoBlob")
	}
}

// A group cannot contain a group. MemberCtx refuses a group member, so nesting
// used to be accepted and then contribute nothing: a group-of-groups answered
// 404 for packages its inner group served, with no error at creation and no log
// — the silent-absence pattern, in the repository model itself.
func TestGroupNesting_IsRefused(t *testing.T) {
	srv := newAdminServer(t)
	mustAdd := func(r repo.Repository) {
		t.Helper()
		if err := srv.Repos.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(repo.Repository{Name: "inner-host", Format: "npm", Kind: repo.Hosted, Enabled: true})
	mustAdd(repo.Repository{Name: "inner-group", Format: "npm", Kind: repo.Group, Enabled: true,
		Members: []string{"inner-host"}})

	for _, tc := range []struct {
		desc    string
		group   repo.Repository
		wantMsg string
	}{
		{"a group member", repo.Repository{Name: "outer", Format: "npm", Kind: repo.Group,
			Members: []string{"inner-group"}}, "cannot be nested"},
		{"itself as a member", repo.Repository{Name: "selfy", Format: "npm", Kind: repo.Group,
			Members: []string{"selfy"}}, "lists itself"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			msg := validateGroupPolicy(tc.group, srv.Repos)
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("validateGroupPolicy = %q, want it to mention %q", msg, tc.wantMsg)
			}
		})
	}

	// A group over hosted and proxy members is still fine.
	mustAdd(repo.Repository{Name: "inner-proxy", Format: "npm", Kind: repo.Proxy,
		Enabled: true, Upstream: "https://registry.npmjs.org"})
	ok := repo.Repository{Name: "fine", Format: "npm", Kind: repo.Group,
		Members: []string{"inner-host", "inner-proxy"}}
	if msg := validateGroupPolicy(ok, srv.Repos); msg != "" {
		t.Errorf("an ordinary group was refused: %s", msg)
	}
}

// noOverwrite is the build-cache shape: no overwrite on any route, but removal
// stays possible and frees the coordinate for a fresh write.
func TestNoOverwrite_ProtocolPath(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "cache", "cran")
	rp, _ := s.Repos.Get("cache")
	rp.NoOverwrite = true
	if err := s.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}
	v1 := tgzWithFile(t, "pkg/DESCRIPTION", "Package: pkg\nVersion: 1.0\nLicense: MIT\n")
	v1b := tgzWithFile(t, "pkg/DESCRIPTION", "Package: pkg\nVersion: 1.0\nLicense: GPL\n")
	do := func(method string, body []byte) int {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req := httptest.NewRequest(method, "/repository/cache/src/contrib/pkg_1.0.tar.gz", r)
		rw := httptest.NewRecorder()
		s.Routes().ServeHTTP(rw, req)
		return rw.Code
	}
	if c := do(http.MethodPut, v1); c != http.StatusCreated {
		t.Fatalf("first PUT: %d", c)
	}
	if c := do(http.MethodPut, v1b); c != http.StatusConflict {
		t.Fatalf("overwriting PUT: got %d, want 409", c)
	}
	if got := targetBlob(t, s, "cache/src/contrib/pkg_1.0.tar.gz"); !bytes.Equal(got, v1) {
		t.Fatal("the refused PUT changed the stored bytes")
	}
	if c := do(http.MethodDelete, nil); c != http.StatusNoContent {
		t.Fatalf("DELETE on a noOverwrite repo: got %d, want 204", c)
	}
	if c := do(http.MethodPut, v1b); c != http.StatusCreated {
		t.Fatalf("PUT after removal: got %d, want 201", c)
	}
}

func TestRepoBlob_NoOverwrite(t *testing.T) {
	srv := newAdminServer(t)
	cache := repo.Repository{Name: "cache", Format: "cran", Kind: repo.Hosted, NoOverwrite: true}
	if _, err := srv.repoBlob(cache).Put("cache/a", strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.repoBlob(cache).Put("cache/a", strings.NewReader("two")); !blob.Refused(err) {
		t.Errorf("noOverwrite repo accepted an overwrite through repoBlob: %v", err)
	}
	if err := srv.repoBlob(cache).Delete("cache/a"); err != nil {
		t.Errorf("noOverwrite repo refused a delete: %v", err)
	}
	// The setting is hosted-only, like Immutable.
	proxy := repo.Repository{Name: "px", Format: "cran", Kind: repo.Proxy, NoOverwrite: true}
	srv.repoBlob(proxy).Put("px/a", strings.NewReader("one")) //nolint:errcheck
	if _, err := srv.repoBlob(proxy).Put("px/a", strings.NewReader("two")); err != nil {
		t.Errorf("noOverwrite must not apply to a proxy: %v", err)
	}
}

func TestPromote_NoOverwriteTargetRejectsOverwrite(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "cran-stage", "cran")
	addHosted(t, s, "cran-cache", "cran")
	body := tgzWithFile(t, "mypkg/DESCRIPTION", "Package: mypkg\nVersion: 1.0\nLicense: MIT\n")
	seedBlobBytes(t, s, http.MethodPut, "cran-stage", "src/contrib/mypkg_1.0.tar.gz", body)
	seedBlobBytes(t, s, http.MethodPut, "cran-cache", "src/contrib/mypkg_1.0.tar.gz", body)
	tgt, _ := s.Repos.Get("cran-cache")
	tgt.NoOverwrite = true
	s.Repos.Update(tgt) //nolint:errcheck
	src, _ := s.Repos.Get("cran-stage")
	_, _, err := s.promoteComponent(context.Background(), "t", src, tgt, "mypkg", "1.0", "http://localhost")
	if pe, ok := err.(*promoteError); !ok || pe.Status != http.StatusConflict {
		t.Fatalf("want 409 promoteError, got %v", err)
	}
}

func TestTrashRestore_OverNewerUploadIs409(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "cache", "cran")
	rp, _ := s.Repos.Get("cache")
	rp.NoOverwrite = true
	s.Repos.Update(rp) //nolint:errcheck
	body := tgzWithFile(t, "pkg/DESCRIPTION", "Package: pkg\nVersion: 1.0\nLicense: MIT\n")
	seedBlobBytes(t, s, http.MethodPut, "cache", "src/contrib/pkg_1.0.tar.gz", body)
	ts, err := cleanup.TrashVersion("cache", "cran", "pkg", "1.0", "t", s.Blob, s.Meta)
	if err != nil {
		t.Fatal(err)
	}
	seedBlobBytes(t, s, http.MethodPut, "cache", "src/contrib/pkg_1.0.tar.gz", body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/cache/trash/restore?id="+ts.ID, nil)
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, req)
	if rw.Code != http.StatusConflict {
		t.Fatalf("restore over a newer upload: got %d %s, want 409", rw.Code, rw.Body)
	}
}

// The trash routes authorize against the repository in the URL, so a trash
// entry must only be reachable through its own repository — otherwise an
// admin of one repository could restore or permanently purge another's.
func TestTrash_EntryOnlyReachableThroughItsRepo(t *testing.T) {
	s := newMigrationServer(t)
	addHosted(t, s, "team-a", "cran")
	addHosted(t, s, "team-b", "cran")
	body := tgzWithFile(t, "pkg/DESCRIPTION", "Package: pkg\nVersion: 1.0\nLicense: MIT\n")
	seedBlobBytes(t, s, http.MethodPut, "team-b", "src/contrib/pkg_1.0.tar.gz", body)
	ts, err := cleanup.TrashVersion("team-b", "cran", "pkg", "1.0", "t", s.Blob, s.Meta)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"restore", "purge"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/team-a/trash/"+action+"?id="+ts.ID, nil)
		rw := httptest.NewRecorder()
		s.Routes().ServeHTTP(rw, req)
		if rw.Code != http.StatusNotFound {
			t.Errorf("%s of team-b's entry via team-a: got %d, want 404", action, rw.Code)
		}
	}
	if _, ok, _ := cleanup.GetTombstone(s.Meta, ts.ID); !ok {
		t.Fatal("team-b's trash entry was consumed through team-a")
	}
}
