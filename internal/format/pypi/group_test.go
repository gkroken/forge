package pypi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// groupSetup builds the arrangement people actually deploy: a hosted repo for
// internal packages and a proxy for pypi.org, behind one group URL.
func groupSetup(t *testing.T) (*Handler, *format.Context, *format.Context, *format.Context) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(dir + "/b")
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(dir + "/m")
	if err != nil {
		t.Fatal(err)
	}
	up := stubUpstream(t)

	mgr := repo.NewManager()
	hosted := repo.Repository{Name: "h", Format: "pypi", Kind: repo.Hosted, Enabled: true}
	prox := repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: up.URL, Enabled: true}
	grp := repo.Repository{Name: "g", Format: "pypi", Kind: repo.Group,
		Members: []string{"h", "p"}, Enabled: true}
	for _, r := range []repo.Repository{hosted, prox, grp} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(r repo.Repository) *format.Context {
		return &format.Context{Repo: r, Blob: b, Meta: m, HTTP: http.DefaultClient, Repos: mgr}
	}
	return New(), mk(hosted), mk(prox), mk(grp)
}

func groupGet(t *testing.T, h *Handler, c *format.Context, sub string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c.Sub = sub
	h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	return w
}

// TestGroup_MergesMembersAndServesFiles — the whole point of a group: one URL
// that answers for both an internal package and an upstream one.
func TestGroup_MergesMembersAndServesFiles(t *testing.T) {
	h, hosted, _, grp := groupSetup(t)
	if code := upload(t, h, hosted, "internal-tool", "1.0.0", "internal_tool-1.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}

	// Upstream-only project, reached through the group.
	w := groupGet(t, h, grp, "simple/six")
	if w.Code != 200 {
		t.Fatalf("group page for an upstream project: %d", w.Code)
	}
	page := w.Body.String()
	if !strings.Contains(page, "six-1.16.0-py2.py3-none-any.whl") {
		t.Errorf("group page missing the upstream file:\n%s", page)
	}
	// Links must address the GROUP, or pip leaves the single URL it was given.
	if !strings.Contains(page, "/repository/g/packages/six/") {
		t.Errorf("group page does not point at the group:\n%s", page)
	}

	// Hosted-only project, same URL.
	w2 := groupGet(t, h, grp, "simple/internal-tool")
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "internal_tool-1.0.0-py3-none-any.whl") {
		t.Errorf("group page for the hosted project: %d\n%s", w2.Code, w2.Body.String())
	}

	// Downloads route to whichever member holds the bytes.
	for _, tc := range []struct{ desc, sub, want string }{
		{"hosted member", "packages/internal-tool/internal_tool-1.0.0-py3-none-any.whl", "bytes"},
		{"proxy member", "packages/six/six-1.16.0-py2.py3-none-any.whl", "WHEEL-BYTES"},
	} {
		w := groupGet(t, h, grp, tc.sub)
		if w.Code != 200 || w.Body.String() != tc.want {
			t.Errorf("%s download: %d %q, want %q", tc.desc, w.Code, w.Body.String(), tc.want)
		}
	}

	// The group index enumerates what it can: hosted projects.
	w3 := groupGet(t, h, grp, "simple")
	if !strings.Contains(w3.Body.String(), "internal-tool") {
		t.Errorf("group index missing the hosted project:\n%s", w3.Body.String())
	}
}

// TestGroup_HostedShadowsProxy is the dependency-confusion case: when the same
// name exists internally and upstream, the group must serve the internal one
// and must not advertise the upstream files at all.
func TestGroup_HostedShadowsProxy(t *testing.T) {
	h, hosted, _, grp := groupSetup(t)
	// "six" also exists upstream in the stub.
	if code := upload(t, h, hosted, "six", "99.0.0", "six-99.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}

	page := groupGet(t, h, grp, "simple/six").Body.String()
	if !strings.Contains(page, "six-99.0.0-py3-none-any.whl") {
		t.Errorf("group dropped the internal package:\n%s", page)
	}
	if strings.Contains(page, "six-1.16.0") || strings.Contains(page, "six-1.15.0") {
		t.Errorf("upstream files served under a name a hosted member owns — "+
			"this is the dependency-confusion hole the merge exists to close:\n%s", page)
	}
	// And the bytes come from the hosted member.
	w := groupGet(t, h, grp, "packages/six/six-99.0.0-py3-none-any.whl")
	if w.Code != 200 || w.Body.String() != "bytes" {
		t.Errorf("download = %d %q, want the hosted bytes", w.Code, w.Body.String())
	}
}

// TestGroup_ClaimShadowsProxy — a claim shadows an upstream name even when the
// hosted member has not published it yet, which is when the attack lands.
func TestGroup_ClaimShadowsProxy(t *testing.T) {
	h, _, _, grp := groupSetup(t)
	grp.NameClaimed = func(name string) bool { return name == "six" }

	w := groupGet(t, h, grp, "simple/six")
	if w.Code != 404 {
		t.Errorf("claimed name served from upstream: %d\n%s", w.Code, w.Body.String())
	}
}

// TestGroup_ReadOnly — publishing goes to the hosted member, not the group.
func TestGroup_ReadOnly(t *testing.T) {
	h, _, _, grp := groupSetup(t)
	w := httptest.NewRecorder()
	grp.Sub = ""
	h.Serve(w, httptest.NewRequest("POST", "/", nil), grp)
	if w.Code != 405 {
		t.Errorf("publish to group = %d, want 405", w.Code)
	}
}

// TestGroup_SurvivesAnUnreachableMember — one bad member must not take the
// group down; the others can still serve.
func TestGroup_SurvivesAnUnreachableMember(t *testing.T) {
	h, hosted, prox, grp := groupSetup(t)
	if code := upload(t, h, hosted, "internal-tool", "1.0.0", "internal_tool-1.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	prox.Repo.Upstream = "http://127.0.0.1:1" // nothing listening
	if err := grp.Repos.Update(repo.Repository{
		Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: "http://127.0.0.1:1", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := groupGet(t, h, grp, "simple/internal-tool")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "internal_tool-1.0.0") {
		t.Errorf("a dead proxy member broke the whole group: %d\n%s", w.Code, w.Body.String())
	}
}

// TestGroup_HostedShadowsProxyListedFirst — the merge collects every member
// before deciding, so a hosted member shadows a proxy even when the proxy comes
// first in the member list. A one-pass merge would serve the upstream package
// here, which is the dependency-confusion case with the members in the less
// careful order.
func TestGroup_HostedShadowsProxyListedFirst(t *testing.T) {
	h, hosted, _, grp := groupSetup(t)
	if err := grp.Repos.Update(repo.Repository{
		Name: "g", Format: "pypi", Kind: repo.Group,
		Members: []string{"p", "h"}, Enabled: true, // proxy FIRST
	}); err != nil {
		t.Fatal(err)
	}
	grp.Repo.Members = []string{"p", "h"}

	if code := upload(t, h, hosted, "six", "99.0.0", "six-99.0.0-py3-none-any.whl"); code != 200 {
		t.Fatalf("upload: %d", code)
	}

	page := groupGet(t, h, grp, "simple/six").Body.String()
	if !strings.Contains(page, "six-99.0.0-py3-none-any.whl") {
		t.Errorf("internal package missing:\n%s", page)
	}
	if strings.Contains(page, "six-1.16.0") {
		t.Errorf("upstream shadowed the internal package because it was listed first:\n%s", page)
	}
}
