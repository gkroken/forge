package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/meta"
	"forge/internal/repo"
)

// A group registry is what `docker pull` points at: internal images and
// upstream ones behind one URL. Manifests and blobs come from whichever member
// holds them; tags/list is the one place a merge — and therefore shadowing —
// happens.

// stubRegistry stands in for an upstream registry: it serves a tag list and a
// manifest for "app".
func stubRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "app", "tags": []string{"upstream-only", "latest"}})
	})
	mux.HandleFunc("/v2/app/manifests/upstream-only", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2,"origin":"upstream"}`))
	})
	// An image no hosted member has, to show ownership only bites for names
	// the hosted member actually holds.
	mux.HandleFunc("/v2/other/tags/list", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "other", "tags": []string{"1.0", "2.0"}})
	})
	mux.HandleFunc("/v2/other/manifests/1.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2,"origin":"upstream"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func groupSetup(t *testing.T) (*Handler, *format.Context, *format.Context) {
	t.Helper()
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	up := stubRegistry(t)

	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "h", Format: "oci", Kind: repo.Hosted, Enabled: true},
		{Name: "p", Format: "oci", Kind: repo.Proxy, Upstream: up.URL, Enabled: true},
		{Name: "g", Format: "oci", Kind: repo.Group, Members: []string{"h", "p"}, Enabled: true},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(name string) *format.Context {
		r, _ := mgr.Get(name)
		return &format.Context{Repo: r, Blob: b, Meta: m, HTTP: up.Client(), Repos: mgr}
	}
	return New(), mk("h"), mk("g")
}

// pushImage puts a one-layer image into a hosted context and returns its tag.
func pushImage(t *testing.T, h *Handler, c *format.Context, image, tag, marker string) {
	t.Helper()
	cfg := []byte(`{"architecture":"amd64","os":"linux","marker":"` + marker + `"}`)
	sum := sha256.Sum256(cfg)
	dgst := "sha256:" + hex.EncodeToString(sum[:])

	c.Sub = fmt.Sprintf("%s/blobs/uploads/?digest=%s", image, dgst)
	w := httptest.NewRecorder()
	h.Serve(w, httptest.NewRequest(http.MethodPost, "/?digest="+dgst, strings.NewReader(string(cfg))), c)
	if w.Code >= 300 {
		t.Fatalf("blob upload: %d %s", w.Code, w.Body.String())
	}
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`,
		dgst, len(cfg))
	c.Sub = image + "/manifests/" + tag
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	h.Serve(w, req, c)
	if w.Code >= 300 {
		t.Fatalf("manifest push: %d %s", w.Code, w.Body.String())
	}
}

func groupGet(t *testing.T, h *Handler, c *format.Context, sub string) *httptest.ResponseRecorder {
	t.Helper()
	c.Sub = sub
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	h.Serve(w, req, c)
	return w
}

// TestGroup_ServesFromHostedAndProxyMembers — before this existed, a group
// answered MANIFEST_UNKNOWN for images its own members held.
func TestGroup_ServesFromHostedAndProxyMembers(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "app", "internal", "hosted")

	w := groupGet(t, h, grp, "app/manifests/internal")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "config") {
		t.Errorf("hosted member's image through the group: %d %s", w.Code, w.Body.String())
	}

	// "other" is not held by the hosted member, so the proxy answers for it.
	// (An upstream tag of an image the hosted member DOES own is deliberately
	// refused — see TestGroup_HostedOwnershipHidesUpstreamImage.)
	w = groupGet(t, h, grp, "other/manifests/1.0")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "upstream") {
		t.Errorf("proxy member's image through the group: %d %s", w.Code, w.Body.String())
	}

	w = groupGet(t, h, grp, "app/manifests/nope")
	if w.Code != 404 {
		t.Errorf("unknown tag = %d, want 404", w.Code)
	}
}

// TestGroup_BlobsRouteToWhicheverMemberHasThem — a blob is addressed by its own
// digest, so any member holding it is the right answer.
func TestGroup_BlobsRouteByDigest(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "app", "internal", "hosted")

	var man map[string]any
	if err := json.Unmarshal(groupGet(t, h, grp, "app/manifests/internal").Body.Bytes(), &man); err != nil {
		t.Fatal(err)
	}
	dgst := man["config"].(map[string]any)["digest"].(string)

	w := groupGet(t, h, grp, "app/blobs/"+dgst)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"marker":"hosted"`) {
		t.Errorf("blob through the group: %d %s", w.Code, w.Body.String())
	}
	if w := groupGet(t, h, grp, "app/blobs/sha256:"+strings.Repeat("0", 64)); w.Code != 404 {
		t.Errorf("unknown blob = %d, want 404", w.Code)
	}
}

// TestGroup_HostedOwnershipHidesUpstreamImage — once a hosted member holds an
// image NAME, proxy members stop answering for that name at all: not just in
// the tag listing, but for manifest pulls too.
//
// Hiding a tag only in tags/list would be theatre — `docker pull` resolves a
// tag directly and never reads the listing, so the "hidden" upstream image
// would still pull. This is the container-image form of the dependency
// confusion the other formats already close on their indexes.
func TestGroup_HostedOwnershipHidesUpstreamImage(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "app", "latest", "hosted")

	var doc struct {
		Tags []string `json:"tags"`
	}
	w := groupGet(t, h, grp, "app/tags/list")
	if w.Code != 200 {
		t.Fatalf("tags/list: %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tag := range doc.Tags {
		if tag == "upstream-only" {
			t.Errorf("upstream tag listed for an image the hosted member owns: %v", doc.Tags)
		}
	}
	if len(doc.Tags) != 1 || doc.Tags[0] != "latest" {
		t.Errorf("tags = %v, want just the hosted tag", doc.Tags)
	}

	// The hiding has to be real, not cosmetic: the upstream tag must not pull.
	if w := groupGet(t, h, grp, "app/manifests/upstream-only"); w.Code != 404 {
		t.Errorf("upstream tag of an owned image still pulls (%d) — hiding it only in "+
			"tags/list would be theatre, since docker pull never reads that listing:\n%s",
			w.Code, w.Body.String())
	}
	// And the same-named tag serves the internal image, not the public one.
	if body := groupGet(t, h, grp, "app/manifests/latest").Body.String(); strings.Contains(body, `"origin":"upstream"`) {
		t.Errorf("group served the upstream image over the internal one of the same name:\n%s", body)
	}
}

// TestGroup_UnownedImagesComeFromUpstream — ownership must bite only for names
// the hosted member actually holds, or a group stops being a proxy at all.
func TestGroup_UnownedImagesComeFromUpstream(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "app", "latest", "hosted")

	w := groupGet(t, h, grp, "other/tags/list")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "2.0") {
		t.Errorf("tags for an unowned image: %d %s", w.Code, w.Body.String())
	}
	if w := groupGet(t, h, grp, "other/manifests/1.0"); w.Code != 200 {
		t.Errorf("manifest for an unowned image: %d %s", w.Code, w.Body.String())
	}
}

// TestGroup_ReadOnly — pushes go to the hosted member, not the group.
func TestGroup_ReadOnly(t *testing.T) {
	h, _, grp := groupSetup(t)
	grp.Sub = "app/manifests/x"
	w := httptest.NewRecorder()
	h.Serve(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader("{}")), grp)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("push to group = %d, want 405", w.Code)
	}
}

// TestGroup_SurvivesDeadProxyMember — one unreachable member must not take the
// group down.
func TestGroup_SurvivesDeadProxyMember(t *testing.T) {
	h, hosted, grp := groupSetup(t)
	pushImage(t, h, hosted, "app", "internal", "hosted")
	if err := grp.Repos.Update(repo.Repository{Name: "p", Format: "oci", Kind: repo.Proxy,
		Upstream: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if w := groupGet(t, h, grp, "app/manifests/internal"); w.Code != 200 {
		t.Errorf("dead proxy member broke the group: %d", w.Code)
	}
	if w := groupGet(t, h, grp, "app/tags/list"); w.Code != 200 || !strings.Contains(w.Body.String(), "internal") {
		t.Errorf("tags/list with a dead member: %d %s", w.Code, w.Body.String())
	}
}
