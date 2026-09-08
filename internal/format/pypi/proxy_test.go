package pypi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/repo"
)

// A stand-in for pypi.org: the index and the files live on different hosts
// there, which is the whole reason forge records a mapping instead of appending
// a sub-path to one upstream prefix. Both are served here, but the page links to
// absolute URLs the way the real one does.
func stubUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".whl.metadata"):
			w.Write([]byte("Metadata-Version: 2.1\nName: six\n")) //nolint:errcheck
		case strings.HasSuffix(r.URL.Path, ".whl"):
			w.Write([]byte("WHEEL-BYTES")) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/simple/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/simple/six/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// Deliberately includes a hostile filename and an unusable extension:
		// upstream is not trusted more than a publisher is.
		w.Write([]byte(`<!DOCTYPE html><html><body>
<a href="` + srv.URL + `/files/aa/bb/six-1.16.0-py2.py3-none-any.whl#sha256=abc123" data-requires-python="&gt;=2.7" data-core-metadata="sha256=deadbeef">six-1.16.0-py2.py3-none-any.whl</a><br />
<a href="` + srv.URL + `/files/cc/dd/six-1.15.0-py2.py3-none-any.whl#sha256=def456" data-yanked="broken sdist">six-1.15.0-py2.py3-none-any.whl</a><br />
<a href="` + srv.URL + `/files/ee/ff/evil&quot;&gt;&lt;img src=x&gt;.whl#sha256=bad">evil</a><br />
<a href="` + srv.URL + `/files/gg/hh/six-1.0.0.exe#sha256=nope">six-1.0.0.exe</a><br />
</body></html>`)) //nolint:errcheck
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProxy_RewritesLinksAndServesFiles(t *testing.T) {
	up := stubUpstream(t)
	h, c := New(), securityCtx(t)
	c.Repo = repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: up.URL}
	c.HTTP = http.DefaultClient

	// --- the simple page ---
	w := httptest.NewRecorder()
	c.Sub = "simple/six"
	h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	if w.Code != 200 {
		t.Fatalf("simple page: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	page := w.Body.String()

	// Links must point back at forge, never at upstream, or pip downloads
	// around the cache and the proxy is decorative.
	if strings.Contains(page, up.URL) {
		t.Errorf("rewritten page still links to upstream:\n%s", page)
	}
	for _, want := range []string{
		"/repository/p/packages/six/six-1.16.0-py2.py3-none-any.whl#sha256=abc123",
		`data-requires-python="&gt;=2.7"`,
		`data-yanked="broken sdist"`,
		`data-core-metadata="sha256=deadbeef"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rewritten page missing %q:\n%s", want, page)
		}
	}
	// Upstream is not more trusted than a publisher: the same filename rules
	// apply, so neither the hostile name nor the uninstallable one survives.
	for _, bad := range []string{"<img", "evil", ".exe"} {
		if strings.Contains(page, bad) {
			t.Errorf("rewritten page carried %q through from upstream:\n%s", bad, page)
		}
	}

	// --- the file, through the mapping ---
	w2 := httptest.NewRecorder()
	c.Sub = "packages/six/six-1.16.0-py2.py3-none-any.whl"
	h.Serve(w2, httptest.NewRequest("GET", "/", nil), c)
	if w2.Code != 200 || w2.Body.String() != "WHEEL-BYTES" {
		t.Errorf("file fetch: got %d %q", w2.Code, w2.Body.String())
	}

	// --- the .metadata sidecar pip asks for when it sees data-core-metadata ---
	w3 := httptest.NewRecorder()
	c.Sub = "packages/six/six-1.16.0-py2.py3-none-any.whl.metadata"
	h.Serve(w3, httptest.NewRequest("GET", "/", nil), c)
	if w3.Code != 200 || !strings.Contains(w3.Body.String(), "Metadata-Version") {
		t.Errorf("metadata sidecar: got %d %q — advertising data-core-metadata without serving it breaks pip resolution",
			w3.Code, w3.Body.String())
	}
}

// TestProxy_FileWithoutIndexIsNotFetched — forge only ever fetches URLs upstream
// handed it. A file nothing has indexed has no mapping, and forge must not
// invent one.
func TestProxy_FileWithoutIndexIsNotFetched(t *testing.T) {
	up := stubUpstream(t)
	h, c := New(), securityCtx(t)
	c.Repo = repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: up.URL}
	c.HTTP = http.DefaultClient

	w := httptest.NewRecorder()
	c.Sub = "packages/six/six-1.16.0-py2.py3-none-any.whl"
	h.Serve(w, httptest.NewRequest("GET", "/", nil), c)
	if w.Code != 404 {
		t.Errorf("got %d, want 404 for a file whose index page was never read", w.Code)
	}
}

// TestProxy_ReadOnlyAndRootIndex — publishing belongs to the hosted repo, and
// the 45 MB root index is refused with an explanation rather than proxied.
func TestProxy_ReadOnlyAndRootIndex(t *testing.T) {
	h, c := New(), securityCtx(t)
	c.Repo = repo.Repository{Name: "p", Format: "pypi", Kind: repo.Proxy, Upstream: "http://example.invalid"}
	c.HTTP = http.DefaultClient

	w := httptest.NewRecorder()
	c.Sub = ""
	h.Serve(w, httptest.NewRequest("POST", "/", nil), c)
	if w.Code != 405 {
		t.Errorf("publish to proxy: got %d, want 405", w.Code)
	}

	w2 := httptest.NewRecorder()
	c.Sub = "simple"
	h.Serve(w2, httptest.NewRequest("GET", "/", nil), c)
	if w2.Code != 501 || !strings.Contains(w2.Body.String(), "request a project directly") {
		t.Errorf("root index: got %d %q", w2.Code, w2.Body.String())
	}
}
