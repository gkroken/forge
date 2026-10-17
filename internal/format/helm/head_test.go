package helm

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
)

// HEAD answers what GET would, without the body (forge-issues/009 found it
// for CRAN; helm answered 404).
func TestHead_AnswersWhatGetWould(t *testing.T) {
	c := newHostedCtx(t)
	if rw := serve(c, http.MethodPost, "api/charts", bytes.NewReader(makeChart(t, "mychart", "0.1.0"))); rw.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rw.Code, rw.Body)
	}
	for _, tc := range []struct {
		sub      string
		download bool
	}{{"mychart-0.1.0.tgz", true}, {"index.yaml", false}, {"api/charts", false}, {"api/charts/mychart", false}} {
		get, head := serve(c, http.MethodGet, tc.sub, nil), serve(c, http.MethodHead, tc.sub, nil)
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
