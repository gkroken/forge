package npm_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/npm"
	"forge/internal/meta"
	"forge/internal/repo"
)

// npm is the one format that takes a path component from the request BODY —
// the _attachments key — rather than the URL. net/http cleans "../" out of a
// URL before routing; nothing cleans a JSON field. Using it to build a blob key
// let a publisher with write access to ONE repository overwrite another
// repository's cached artifacts, which the group then served to every
// developer: a supply-chain hole reachable by anyone who could publish.

func publishCtx(t *testing.T, name string, b blob.Store, m meta.Store) *format.Context {
	t.Helper()
	return &format.Context{
		Repo: repo.Repository{Name: name, Format: "npm", Kind: repo.Hosted},
		Blob: b, Meta: m,
	}
}

func publish(t *testing.T, c *format.Context, pkg, version, attachmentName, body string) int {
	t.Helper()
	doc := map[string]any{
		"name": pkg,
		"versions": map[string]any{version: map[string]any{
			"name": pkg, "version": version, "dist": map[string]any{}}},
		"_attachments": map[string]any{attachmentName: map[string]any{
			"data": base64.StdEncoding.EncodeToString([]byte(body))}},
	}
	raw, _ := json.Marshal(doc)
	c.Sub = pkg
	w := httptest.NewRecorder()
	npm.New().Serve(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(string(raw))), c)
	return w.Code
}

// TestPublish_AttachmentNameCannotEscapeTheRepository is the regression for the
// vulnerability: the write must not land outside the publishing repo.
func TestPublish_AttachmentNameCannotEscapeTheRepository(t *testing.T) {
	dir := t.TempDir()
	b, err := blob.NewFS(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := meta.NewFS(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	victim := publishCtx(t, "npmjs", b, m)
	if _, err := victim.Blob.Put("npmjs/is-odd/-/is-odd-3.0.1.tgz",
		strings.NewReader("GENUINE-UPSTREAM-TARBALL")); err != nil {
		t.Fatal(err)
	}

	attacker := publishCtx(t, "acme-npm", b, m)
	for _, name := range []string{
		"../../../npmjs/is-odd/-/is-odd-3.0.1.tgz", // over another repo's cache
		"../../../../pwned-1.0.0.tgz",              // out to the blob root
		"../evil-1.0.0.tgz",                        // one level up
		"sub/dir/evil-1.0.0.tgz",                   // any separator at all
	} {
		code := publish(t, attacker, "travprobe", "1.0.0", name, "MALICIOUS")
		if code < 400 {
			t.Errorf("attachment %q was accepted (HTTP %d); it must be refused", name, code)
		}
	}

	// The victim's artifact is untouched.
	rc, err := victim.Blob.Get("npmjs/is-odd/-/is-odd-3.0.1.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close() //nolint:errcheck
	buf := make([]byte, 64)
	n, _ := rc.Read(buf)
	if got := string(buf[:n]); got != "GENUINE-UPSTREAM-TARBALL" {
		t.Errorf("another repository's cached tarball was overwritten: %q", got)
	}

	// Nothing landed outside a repository prefix.
	keys, _ := b.List("")
	for _, k := range keys {
		if !strings.HasPrefix(k, "npmjs/") && !strings.HasPrefix(k, "acme-npm/") {
			t.Errorf("blob written outside any repository: %q", k)
		}
	}
}

// TestPublish_ScopedPackageIsStoredWhereItIsAdvertised — npm names a scoped
// attachment "@scope/name-1.0.0.tgz" but the packument advertises
// "name-1.0.0.tgz". Storing under the attachment name published scoped packages
// to a path no client ever requests: npm install returned 404 for every one.
func TestPublish_ScopedPackageIsStoredWhereItIsAdvertised(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := publishCtx(t, "acme-npm", b, m)

	if code := publish(t, c, "@acme/toolkit", "1.0.0", "@acme/toolkit-1.0.0.tgz", "TARBALL"); code >= 300 {
		t.Fatalf("publish: %d", code)
	}

	// Whatever the packument advertises must be fetchable.
	c.Sub = "@acme/toolkit"
	w := httptest.NewRecorder()
	npm.New().Serve(w, httptest.NewRequest(http.MethodGet, "/", nil), c)
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	tarball := doc.Versions["1.0.0"].Dist.Tarball
	if tarball == "" {
		t.Fatal("packument advertises no tarball")
	}
	i := strings.Index(tarball, "/repository/acme-npm/")
	if i < 0 {
		t.Fatalf("unexpected tarball URL %q", tarball)
	}
	sub := tarball[i+len("/repository/acme-npm/"):]

	c.Sub = sub
	w = httptest.NewRecorder()
	npm.New().Serve(w, httptest.NewRequest(http.MethodGet, "/", nil), c)
	if w.Code != 200 || w.Body.String() != "TARBALL" {
		t.Errorf("the advertised tarball URL %q returns %d — a scoped package "+
			"publishes but cannot be installed", sub, w.Code)
	}
}

// TestPublish_UnscopedStillWorks — the attachment names npm actually sends for
// unscoped packages must keep working.
func TestPublish_UnscopedStillWorks(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := publishCtx(t, "acme-npm", b, m)

	for _, tc := range []struct{ pkg, ver, attachment string }{
		{"is-odd", "3.0.1", "is-odd-3.0.1.tgz"},
		{"@acme/toolkit", "2.0.0", "toolkit-2.0.0.tgz"}, // the other spelling
		{"left-pad", "1.3.0-beta.1", "left-pad-1.3.0-beta.1.tgz"},
	} {
		if code := publish(t, c, tc.pkg, tc.ver, tc.attachment, "OK"); code >= 300 {
			t.Errorf("publish %s@%s via %q refused: %d", tc.pkg, tc.ver, tc.attachment, code)
			continue
		}
		key := fmt.Sprintf("acme-npm/%s/-/%s-%s.tgz", tc.pkg, tc.pkg[strings.LastIndex(tc.pkg, "/")+1:], tc.ver)
		if _, exists, _ := b.Stat(key); !exists {
			t.Errorf("%s@%s not stored at the advertised key %s", tc.pkg, tc.ver, key)
		}
	}
}
