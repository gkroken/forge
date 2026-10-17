package pypi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// HEAD answers what GET would, without the body (forge-issues/009 found it
// for CRAN; a hosted pypi repository answered 404).
func TestHead_AnswersWhatGetWould(t *testing.T) {
	h, c := New(), securityCtx(t)
	if code := upload(t, h, c, "demo", "1.0", "demo-1.0.tar.gz"); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	call := func(method, sub string) *httptest.ResponseRecorder {
		cc := *c
		cc.Sub = sub
		rw := httptest.NewRecorder()
		h.Serve(rw, httptest.NewRequest(method, "/", nil), &cc)
		return rw
	}
	for _, tc := range []struct {
		sub      string
		download bool
	}{{"packages/demo/demo-1.0.tar.gz", true}, {"simple/", false}, {"simple/demo/", false}} {
		get, head := call(http.MethodGet, tc.sub), call(http.MethodHead, tc.sub)
		if get.Code != http.StatusOK || head.Code != get.Code {
			t.Errorf("%s: HEAD %d, GET %d", tc.sub, head.Code, get.Code)
			continue
		}
		if head.Header().Get("Content-Type") != get.Header().Get("Content-Type") {
			t.Errorf("%s: HEAD Content-Type %q, GET %q", tc.sub, head.Header().Get("Content-Type"), get.Header().Get("Content-Type"))
		}
		if tc.download && (head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) || head.Body.Len() != 0) {
			t.Errorf("%s: HEAD Content-Length %q (GET %d bytes), body %d bytes", tc.sub, head.Header().Get("Content-Length"), get.Body.Len(), head.Body.Len())
		}
	}
}
