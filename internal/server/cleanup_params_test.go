package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The cleanup endpoint deletes artifacts. Previewing a run with "?dryRun=true"
// — not a real parameter; the flag is "?dry=true" — silently performed a live
// cleanup, because an unrecognised query parameter was simply ignored. A typo
// like "?dry=ture" behaves the same way. On a destructive route that has to be
// an error.
func TestCleanupRejectsUnknownQueryParams(t *testing.T) {
	env := newAuthEnv(t)
	for _, q := range []string{"?dryRun=true", "?dry=true&extra=1", "?dry-run=true", "?DRY=true"} {
		t.Run(q, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+env.repo+"/cleanup"+q, nil)
			r.Header.Set("Authorization", "Bearer "+env.adminToken)
			env.srv.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400 — an unrecognised parameter on a destructive "+
					"endpoint must not be silently ignored", q, w.Code)
			}
			if !strings.Contains(w.Body.String(), "unknown query parameter") {
				t.Errorf("body = %s", w.Body.String())
			}
		})
	}
}

// The real flag, and no parameters at all, must still work.
func TestCleanupAcceptsKnownParams(t *testing.T) {
	env := newAuthEnv(t)
	for _, q := range []string{"", "?dry=true", "?dry=false"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+env.repo+"/cleanup"+q, nil)
		r.Header.Set("Authorization", "Bearer "+env.adminToken)
		env.srv.ServeHTTP(w, r)
		if w.Code == http.StatusBadRequest {
			t.Errorf("cleanup%q was refused: %s", q, w.Body.String())
		}
	}
}
