package server

import (
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
