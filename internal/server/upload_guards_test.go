package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
