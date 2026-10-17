package npm_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format/npm"
	"forge/internal/meta"
)

// HEAD answers what GET would, without the body (forge-issues/009 found it
// for CRAN; npm answered 405).
func TestHead_AnswersWhatGetWould(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	c := publishCtx(t, "npm-hosted", b, m)
	if code := publish(t, c, "lp", "1.0.0", "lp-1.0.0.tgz", "tarball bytes"); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("publish: %d", code)
	}
	call := func(method, sub string) *httptest.ResponseRecorder {
		cc := *c
		cc.Sub = sub
		rw := httptest.NewRecorder()
		npm.New().Serve(rw, httptest.NewRequest(method, "/", nil), &cc)
		return rw
	}
	for _, tc := range []struct {
		sub      string
		download bool
	}{{"lp/-/lp-1.0.0.tgz", true}, {"lp", false}, {"-/package/lp/dist-tags", false}, {"-/ping", false}, {"-/whoami", false}} {
		assertHead(t, tc.sub, call(http.MethodGet, tc.sub), call(http.MethodHead, tc.sub), tc.download)
	}
}

func assertHead(t *testing.T, name string, get, head *httptest.ResponseRecorder, download bool) {
	t.Helper()
	if get.Code != http.StatusOK || head.Code != get.Code {
		t.Errorf("%s: HEAD %d, GET %d", name, head.Code, get.Code)
		return
	}
	if head.Header().Get("Content-Type") != get.Header().Get("Content-Type") {
		t.Errorf("%s: HEAD Content-Type %q, GET %q", name, head.Header().Get("Content-Type"), get.Header().Get("Content-Type"))
	}
	if download {
		if want := strconv.Itoa(get.Body.Len()); head.Header().Get("Content-Length") != want {
			t.Errorf("%s: HEAD Content-Length %q, want %s", name, head.Header().Get("Content-Length"), want)
		}
		if head.Body.Len() != 0 {
			t.Errorf("%s: HEAD sent %d body bytes", name, head.Body.Len())
		}
	}
}

// On a proxy, dist-tags are read-only: a HEAD is a read and is answered, a
// write is still refused (review of 8fd3cff, N1).
func TestHead_ProxyDistTags(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lp" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"lp","dist-tags":{"latest":"1.0.0"},"versions":{"1.0.0":{"name":"lp","version":"1.0.0","dist":{}}}}`)) //nolint:errcheck
	}))
	defer up.Close()
	c := proxyCtx(t, up.URL, up.Client())
	call := func(method string) *httptest.ResponseRecorder {
		cc := *c
		cc.Sub = "-/package/lp/dist-tags/latest"
		rw := httptest.NewRecorder()
		npm.New().Serve(rw, httptest.NewRequest(method, "/", strings.NewReader(`"9.9.9"`)), &cc)
		return rw
	}
	assertHead(t, "proxy dist-tags", call(http.MethodGet), call(http.MethodHead), false)
	if rw := call(http.MethodPut); rw.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on a proxy's dist-tags: %d, want 405", rw.Code)
	}
}
