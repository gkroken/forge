package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/cranplatform"
)

func platformJSON(key string) map[string]any {
	return map[string]any{
		"key":           key,
		"rPlatform":     "x86_64-pc-linux-gnu",
		"builderImage":  "ghcr.io/acme/r-builder:4.6.1-noble",
		"builderDigest": "sha256:" + strings.Repeat("cd", 32),
		"maxGlibc":      "2.39",
		"libraries":     []string{"libc.so.6", "libR.so"},
	}
}

func platformReq(t *testing.T, srv *Server, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body) //nolint:errcheck
	}
	r := httptest.NewRequest(method, path, &buf)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rw := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rw, r)
	return rw
}

func TestCRANPlatformsAPI_CRUD(t *testing.T) {
	srv := newAdminServer(t)
	const p = "/api/v1/cran/platforms"

	if rw := platformReq(t, srv, http.MethodPut, p+"/noble-x86_64/4.6", "", platformJSON("")); rw.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rw.Code, rw.Body)
	}
	rw := platformReq(t, srv, http.MethodGet, p+"/noble-x86_64/4.6", "", nil)
	var got cranplatform.Platform
	json.Unmarshal(rw.Body.Bytes(), &got) //nolint:errcheck
	if rw.Code != http.StatusOK || got.Key != "noble-x86_64/4.6" || !got.Provides("libR.so") {
		t.Fatalf("get: %d %+v", rw.Code, got)
	}
	var list []cranplatform.Platform
	json.Unmarshal(platformReq(t, srv, http.MethodGet, p, "", nil).Body.Bytes(), &list) //nolint:errcheck
	if len(list) != 1 {
		t.Fatalf("list: %+v", list)
	}

	// Invalid registrations and a body/URL mismatch are refused with the reason.
	bad := platformJSON("")
	bad["maxGlibc"] = "latest"
	if rw := platformReq(t, srv, http.MethodPut, p+"/noble-x86_64/4.6", "", bad); rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), "maxGlibc") {
		t.Errorf("invalid registration: %d %s", rw.Code, rw.Body)
	}
	if rw := platformReq(t, srv, http.MethodPut, p+"/noble-x86_64/4.6", "", platformJSON("jammy-x86_64/4.6")); rw.Code != http.StatusBadRequest {
		t.Errorf("mismatched key: %d", rw.Code)
	}
	if rw := platformReq(t, srv, http.MethodPut, p+"/noble-amd64/4.6", "", platformJSON("")); rw.Code != http.StatusBadRequest {
		t.Errorf("bad key in URL: %d", rw.Code)
	}

	if rw := platformReq(t, srv, http.MethodDelete, p+"/noble-x86_64/4.6", "", nil); rw.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rw.Code)
	}
	if rw := platformReq(t, srv, http.MethodGet, p+"/noble-x86_64/4.6", "", nil); rw.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rw.Code)
	}
	if rw := platformReq(t, srv, http.MethodDelete, p+"/noble-x86_64/4.6", "", nil); rw.Code != http.StatusNotFound {
		t.Errorf("delete of unregistered key: %d", rw.Code)
	}
}

// A registration decides what every binary repository accepts for an
// environment, so writing one is global-admin only — a builder's write token
// must not be able to widen its own platform's manifest.
func TestCRANPlatformsAPI_WritesAdminOnly(t *testing.T) {
	srv, store := newAuthServer(t)
	_, writer, err := store.Create("builder", []auth.Grant{auth.GrantForRole("*", auth.RoleWrite)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rw := platformReq(t, srv, method, "/api/v1/cran/platforms/noble-x86_64/4.6", writer, platformJSON(""))
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s by a write token: %d, want 403", method, rw.Code)
		}
	}
	if _, ok, _ := cranplatform.NewStore(srv.Meta).Get("noble-x86_64/4.6"); ok {
		t.Fatal("a non-admin registered a platform")
	}
}

// A client must be able to check a registration names the builder image it
// expects before it installs binaries governed by it, so any authenticated
// caller may read one — but not an anonymous one.
func TestCRANPlatformsAPI_ReadsNeedAuthOnly(t *testing.T) {
	srv, store := newAuthServer(t)
	if err := cranplatform.NewStore(srv.Meta).Put(cranplatform.Platform{
		Key: "noble-x86_64/4.6", RPlatform: "x86_64-pc-linux-gnu",
		BuilderImage: "ghcr.io/acme/r-builder:4.6.1-noble", BuilderDigest: "sha256:" + strings.Repeat("cd", 32),
		MaxGlibc: "2.39", Libraries: []string{"libc.so.6", "libR.so"},
	}); err != nil {
		t.Fatal(err)
	}
	// A reader scoped to one repository: no grant touches the registry itself.
	_, reader, err := store.Create("skald", []auth.Grant{auth.GrantForRole("cran-linux", auth.RoleRead)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const p = "/api/v1/cran/platforms"

	rw := platformReq(t, srv, http.MethodGet, p+"/noble-x86_64/4.6", reader, nil)
	var got cranplatform.Platform
	json.Unmarshal(rw.Body.Bytes(), &got) //nolint:errcheck
	if rw.Code != http.StatusOK || got.BuilderDigest != "sha256:"+strings.Repeat("cd", 32) {
		t.Fatalf("get by a read token: %d %s", rw.Code, rw.Body)
	}
	var list []cranplatform.Platform
	if rw := platformReq(t, srv, http.MethodGet, p, reader, nil); rw.Code != http.StatusOK {
		t.Fatalf("list by a read token: %d", rw.Code)
	} else if json.Unmarshal(rw.Body.Bytes(), &list); len(list) != 1 { //nolint:errcheck
		t.Fatalf("list by a read token: %s", rw.Body)
	}

	for _, path := range []string{p, p + "/noble-x86_64/4.6"} {
		if rw := platformReq(t, srv, http.MethodGet, path, "", nil); rw.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s: %d, want 401", path, rw.Code)
		}
		if rw := platformReq(t, srv, http.MethodGet, path, "not-a-token", nil); rw.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with an invalid token: %d, want 401", path, rw.Code)
		}
	}
}

func TestCRANPlatformsAPI_Methods(t *testing.T) {
	srv := newAdminServer(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/cran/platforms"},
		{http.MethodPatch, "/api/v1/cran/platforms/noble-x86_64/4.6"},
		{http.MethodPost, "/repository/__api__/status"},
	} {
		if rw := platformReq(t, srv, c.method, c.path, "", nil); rw.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 405", c.method, c.path, rw.Code)
		}
	}
	if rw := platformReq(t, srv, http.MethodPut, "/api/v1/cran/platforms/noble-x86_64/4.6", "", "not an object"); rw.Code != http.StatusBadRequest {
		t.Errorf("non-object body: %d", rw.Code)
	}
}
