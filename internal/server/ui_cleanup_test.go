package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestUICleanupPolicy_Lifecycle drives the cleanup-policy UI: create (with a
// repo assignment), edit, per-repo panel, and delete. The create path also
// exercises applyPolicyToRepos and, via a bad interval, reRenderPolicyForm.
func TestUICleanupPolicy_Lifecycle(t *testing.T) {
	srv := newRichUIServer(t)
	h := srv.Routes()

	// Create a policy and assign it to npm-hosted.
	rw := uiPost(t, h, "/ui/admin/cleanup-policies/new", url.Values{
		"name":         {"keep-5"},
		"description":  {"keep last 5"},
		"keepVersions": {"5"},
		"interval":     {"24h"},
		"applyRepos":   {"npm-hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("create policy: status %d (%.200s)", rw.Code, rw.Body.String())
	}
	if _, ok, _ := srv.Cleanup.Get("keep-5"); !ok {
		t.Fatal("policy keep-5 was not persisted")
	}
	// The assignment landed on the repo.
	if rp, _ := srv.Repos.Get("npm-hosted"); rp.CleanupPolicyName != "keep-5" {
		t.Errorf("npm-hosted policy = %q, want keep-5", rp.CleanupPolicyName)
	}

	// Bad interval → form re-renders with an error (200), not a redirect.
	rw = uiPost(t, h, "/ui/admin/cleanup-policies/new", url.Values{
		"name": {"bad"}, "interval": {"not-a-duration"},
	})
	if rw.Code != http.StatusOK {
		t.Errorf("bad interval: status %d, want 200 re-render", rw.Code)
	}

	// Edit the existing policy (GET the edit form, then POST an update).
	rw = uiGet(t, h, "/ui/admin/cleanup-policies/keep-5/edit")
	if rw.Code != http.StatusOK {
		t.Errorf("edit form GET: status %d", rw.Code)
	}
	rw = uiPost(t, h, "/ui/admin/cleanup-policies/keep-5/edit", url.Values{
		"keepVersions": {"10"}, "applyRepos": {"npm-hosted"},
	})
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("edit policy: status %d", rw.Code)
	}
	if p, _, _ := srv.Cleanup.Get("keep-5"); p.KeepVersions != 10 {
		t.Errorf("KeepVersions = %d, want 10 after edit", p.KeepVersions)
	}

	// Per-repo cleanup panel renders.
	rw = uiGet(t, h, "/ui/admin/repos/npm-hosted/cleanup")
	if rw.Code != http.StatusOK {
		t.Errorf("repo cleanup panel: status %d", rw.Code)
	}

	// Delete the policy via htmx (HX-Redirect header, 200).
	rw = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/ui/admin/cleanup-policies/keep-5", nil)
	req.Header.Set("HX-Request", "true")
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("delete policy: status %d", rw.Code)
	}
	if rw.Header().Get("HX-Redirect") == "" {
		t.Error("expected HX-Redirect header on htmx delete")
	}
	if _, ok, _ := srv.Cleanup.Get("keep-5"); ok {
		t.Error("policy keep-5 should be deleted")
	}
}
