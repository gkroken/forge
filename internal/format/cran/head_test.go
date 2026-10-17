package cran

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"forge/internal/format"
)

func serveMethod(c *format.Context, method, sub, ua string) *httptest.ResponseRecorder {
	cc := *c
	cc.Sub = sub
	r := httptest.NewRequest(method, "/", nil)
	if ua != "" {
		r.Header.Set("User-Agent", ua)
	}
	rw := httptest.NewRecorder()
	New().Serve(rw, r, &cc)
	return rw
}

// HEAD answers what GET would, without the body, on every read path
// (forge-issues/009: HEAD on a hosted tarball through the group was 404).
func TestHead_AnswersWhatGetWould(t *testing.T) {
	g := newServeGroup(t)
	g.source(t, "lb", "1.0.2")
	putBinary(t, g.bins, "noble-x86_64/4.6", "lb", "1.0.2")
	win := makeCRANPkg(t, "winpkg", "1.0.0") // any bytes: stored, not validated, for the download path
	if _, err := g.src.Blob.Put(g.src.Key("bin/windows/contrib/4.4/winpkg_1.0.0.zip"), bytes.NewReader(win)); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		c    *format.Context
		sub  string
		ua   string
		body bool // a download: HEAD must carry Content-Length and no body
	}{
		{"hosted tarball", g.src, "src/contrib/lb_1.0.2.tar.gz", "", true},
		{"group tarball (hosted member)", g.group, "src/contrib/lb_1.0.2.tar.gz", uaRocker, true},
		{"group __linux__ source", g.group, lx + "lb_1.0.2.tar.gz", uaLibcurl, true},
		{"group __linux__ binary", g.group, lx + "lb_1.0.2.tar.gz", uaRocker, true},
		{"group latest/ source view", g.group, "latest/src/contrib/lb_1.0.2.tar.gz", "", true},
		{"hosted PACKAGES", g.src, "src/contrib/PACKAGES", "", false},
		{"group PACKAGES.gz", g.group, "src/contrib/PACKAGES.gz", "", false},
		{"group PACKAGES.rds", g.group, "src/contrib/PACKAGES.rds", "", false},
		{"group __linux__ PACKAGES", g.group, lx + "PACKAGES", uaRocker, false},
		{"hosted Windows binary", g.src, "bin/windows/contrib/4.4/winpkg_1.0.0.zip", "", true},
		{"hosted Windows PACKAGES", g.src, "bin/windows/contrib/4.4/PACKAGES", "", false},
		{"binary platform list", g.src, "bin/windows/contrib/", "", false},
	} {
		get := serveMethod(tc.c, http.MethodGet, tc.sub, tc.ua)
		head := serveMethod(tc.c, http.MethodHead, tc.sub, tc.ua)
		if get.Code != http.StatusOK {
			t.Errorf("%s: GET %d", tc.name, get.Code)
			continue
		}
		if head.Code != get.Code {
			t.Errorf("%s: HEAD %d, GET %d", tc.name, head.Code, get.Code)
		}
		for _, h := range []string{"Content-Type", "X-Forge-Package-Type"} {
			if head.Header().Get(h) != get.Header().Get(h) {
				t.Errorf("%s: HEAD %s %q, GET %q", tc.name, h, head.Header().Get(h), get.Header().Get(h))
			}
		}
		if tc.body {
			if want := strconv.Itoa(get.Body.Len()); head.Header().Get("Content-Length") != want {
				t.Errorf("%s: HEAD Content-Length %q, want %s", tc.name, head.Header().Get("Content-Length"), want)
			}
			if head.Body.Len() != 0 {
				t.Errorf("%s: HEAD sent %d body bytes", tc.name, head.Body.Len())
			}
		}
	}
}
