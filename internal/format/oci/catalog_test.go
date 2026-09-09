package oci

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/format"
)

// GET /_catalog lists every image in a registry. It was absent from the op
// switch entirely, so it answered "unknown OCI operation" for all three kinds.

func getCatalog(t *testing.T, h *Handler, c *format.Context, query string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	c.Sub = "_catalog"
	w := httptest.NewRecorder()
	target := "/v2/_catalog"
	if query != "" {
		target += "?" + query
	}
	h.Serve(w, httptest.NewRequest(http.MethodGet, target, nil), c)
	var doc struct {
		Repositories []string `json:"repositories"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	return w, doc.Repositories
}

func TestCatalog_Hosted(t *testing.T) {
	h := New()
	c := newCtx(t)
	for _, img := range []string{"zeta", "alpha", "org/nested"} {
		pushImage(t, h, c, img, "v1", "hosted")
	}

	w, repos := getCatalog(t, h, c, "")
	if w.Code != 200 {
		t.Fatalf("_catalog = %d %s", w.Code, w.Body.String())
	}
	want := []string{"alpha", "org/nested", "zeta"}
	if strings.Join(repos, ",") != strings.Join(want, ",") {
		t.Errorf("repositories = %v, want %v (sorted, and a nested name kept whole)", repos, want)
	}
}

// TestCatalog_Pagination — a registry that ignores "n" hands back everything,
// which is what clients page to avoid.
func TestCatalog_Pagination(t *testing.T) {
	h := New()
	c := newCtx(t)
	for _, img := range []string{"a", "b", "c", "d"} {
		pushImage(t, h, c, img, "v1", "hosted")
	}

	w, repos := getCatalog(t, h, c, "n=2")
	if len(repos) != 2 || repos[0] != "a" || repos[1] != "b" {
		t.Errorf("n=2 gave %v, want the first two", repos)
	}
	if link := w.Header().Get("Link"); !strings.Contains(link, `rel="next"`) || !strings.Contains(link, "last=b") {
		t.Errorf("Link header = %q, want a next link continuing after b", link)
	}

	_, repos = getCatalog(t, h, c, "n=2&last=b")
	if len(repos) != 2 || repos[0] != "c" || repos[1] != "d" {
		t.Errorf("n=2&last=b gave %v, want the next two", repos)
	}

	_, repos = getCatalog(t, h, c, "last=d")
	if len(repos) != 0 {
		t.Errorf("last=d gave %v, want an empty page at the end", repos)
	}
}

func TestCatalog_EmptyRegistryIsAnEmptyList(t *testing.T) {
	h := New()
	c := newCtx(t)
	w, repos := getCatalog(t, h, c, "")
	if w.Code != 200 || len(repos) != 0 {
		t.Errorf("empty registry = %d %s, want 200 with an empty list", w.Code, w.Body.String())
	}
	// null would break clients that range over the field.
	if !strings.Contains(w.Body.String(), "[]") {
		t.Errorf("empty catalog rendered as %s, want []", w.Body.String())
	}
}

func TestCatalog_GroupMergesMembers(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "internal-app", "v1", "hosted")

	w, repos := getCatalog(t, h, grp, "")
	if w.Code != 200 {
		t.Fatalf("group _catalog = %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, r := range repos {
		if r == "internal-app" {
			found = true
		}
	}
	if !found {
		t.Errorf("group catalog %v missing the hosted member's image", repos)
	}
}
