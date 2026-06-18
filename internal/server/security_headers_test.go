package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// requiredSecurityHeaders lists the headers the middleware must set on every
// response. This is the regression guard for the ZAP DAST job: if a header is
// accidentally removed here the DAST baseline scan will catch it in CI, but
// this test catches it immediately on every PR.
var requiredSecurityHeaders = []struct {
	name  string
	value string // empty string means "must be present with any value"
}{
	{"X-Content-Type-Options", "nosniff"},
	{"X-Frame-Options", "DENY"},
	{"Referrer-Policy", "strict-origin-when-cross-origin"},
	{"Cross-Origin-Resource-Policy", "same-site"},
	{"Content-Security-Policy", ""}, // value checked below
}

func TestSecurityHeaders_PresentOnAllRoutes(t *testing.T) {
	srv := newUIServer(t)
	handler := srv.Routes()

	routes := []string{
		"/",
		"/healthz",
		"/readyz",
		"/repository/npm-hosted/lodash",
		"/api/v1/repos",
		"/api/v1/cleanup-policies",
		"/ui/",
	}

	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			handler.ServeHTTP(w, r)

			for _, want := range requiredSecurityHeaders {
				got := w.Header().Get(want.name)
				if got == "" {
					t.Errorf("missing header %s", want.name)
					continue
				}
				if want.value != "" && got != want.value {
					t.Errorf("header %s: got %q want %q", want.name, got, want.value)
				}
			}

			// CSP must contain frame-ancestors and must not allow unsafe-eval.
			csp := w.Header().Get("Content-Security-Policy")
			if csp == "" {
				t.Error("Content-Security-Policy header missing")
			}
			for _, mustContain := range []string{"frame-ancestors 'none'", "default-src"} {
				if !containsStr(csp, mustContain) {
					t.Errorf("CSP missing %q: %s", mustContain, csp)
				}
			}
			if containsStr(csp, "unsafe-eval") {
				t.Errorf("CSP must not allow unsafe-eval: %s", csp)
			}
		})
	}
}

// ── SEC-001: upload size limit ────────────────────────────────────────────────

func TestMaxUpload_ContentLengthRejected(t *testing.T) {
	srv := newUIServer(t)
	srv.MaxUpload = 100 // tiny limit for this test
	h := srv.Routes()

	// Body within limit but Content-Length header declares oversize.
	body := bytes.Repeat([]byte("x"), 10)
	r := httptest.NewRequest(http.MethodPut, "/repository/npm-hosted/pkg", bytes.NewReader(body))
	r.ContentLength = 200 // declared size > limit

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized Content-Length: got %d want 413", w.Code)
	}
}

func TestMaxUpload_ReadTruncated(t *testing.T) {
	srv := newUIServer(t)
	srv.MaxUpload = 50 // 50 bytes
	h := srv.Routes()

	// Send a body larger than the limit with no Content-Length (chunked-style).
	big := bytes.Repeat([]byte("x"), 1000)
	r := httptest.NewRequest(http.MethodPut, "/repository/npm-hosted/pkg", bytes.NewReader(big))
	r.ContentLength = -1 // unknown size

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// Handler must not return 200; exact code (400 or 413) depends on Go version.
	if w.Code == http.StatusOK {
		t.Errorf("oversized body with unknown Content-Length: expected non-200, got 200")
	}
}

func TestMaxUpload_LegitimateBodyAllowed(t *testing.T) {
	srv := newUIServer(t)
	srv.MaxUpload = 1 << 20 // 1 MiB
	h := srv.Routes()

	// A small well-formed JSON body must not be rejected by the size limit.
	body := []byte(`{"name":"x"}`)
	r := httptest.NewRequest(http.MethodPut, "/repository/npm-hosted/x", bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// 413 must not be returned for a body under the limit.
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("small legitimate body rejected with 413")
	}
}

func TestMaxUpload_GetNotLimited(t *testing.T) {
	srv := newUIServer(t)
	srv.MaxUpload = 1 // absurdly small — must not affect GET
	h := srv.Routes()

	r := httptest.NewRequest(http.MethodGet, "/repository/npm-hosted/lodash", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("GET should never trigger upload size limit")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && stringContains(s, sub))
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
