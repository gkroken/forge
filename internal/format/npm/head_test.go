package npm_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
	}{{"lp/-/lp-1.0.0.tgz", true}, {"lp", false}, {"-/package/lp/dist-tags", false}, {"-/ping", false}} {
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
