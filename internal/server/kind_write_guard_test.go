package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/format/pypi"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Only a hosted repository accepts writes. A proxy mirrors someone else's
// registry and a group is a read-only view of its members, so a write to
// either is a write to content forge does not own.
//
// This is one test across every format on purpose. Each format used to answer
// the question route by route, and each hole found so far was a route someone
// added later: npm's dist-tags endpoint repointed "latest" inside a proxy's
// cached packument. A new write route that forgets the guard fails here.
func TestWrites_RefusedOnEveryNonHostedRepository(t *testing.T) {
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	for _, h := range []format.Handler{maven.New(), npm.New(), helm.New(), cran.New(), pypi.New(), oci.New()} {
		reg.Register(h)
	}

	// Every write route each format exposes, by the verb a real client uses.
	writes := map[string][][2]string{
		"maven": {
			{"PUT", "com/acme/widget/1.0.0/widget-1.0.0.jar"},
			{"DELETE", "com/acme/widget/1.0.0/widget-1.0.0.jar"},
		},
		"npm": {
			{"PUT", "leftpad"},
			{"DELETE", "leftpad"},
			{"DELETE", "leftpad/-/leftpad-1.0.0.tgz"},
			{"PUT", "-/package/leftpad/dist-tags/latest"},
			{"DELETE", "-/package/leftpad/dist-tags/latest"},
		},
		"helm": {
			{"POST", "api/charts"},
			{"DELETE", "api/charts/mychart/1.0.0"},
		},
		"cran": {
			{"PUT", "src/contrib/pkg_1.0.0.tar.gz"},
			{"DELETE", "src/contrib/pkg_1.0.0.tar.gz"},
			{"PUT", "bin/windows/contrib/4.3/pkg_1.0.0.zip"},
			{"DELETE", "bin/windows/contrib/4.3/pkg_1.0.0.zip"},
		},
		"pypi": {
			{"POST", ""},
			{"DELETE", "packages/pkg/pkg-1.0.0.tar.gz"},
		},
		"oci": {
			{"PUT", "app/manifests/v1"},
			{"DELETE", "app/manifests/v1"},
			{"DELETE", "app/blobs/sha256:" + strings.Repeat("a", 64)},
			{"POST", "app/blobs/uploads/"},
		},
	}

	for _, f := range []string{"maven", "npm", "helm", "cran", "pypi", "oci"} {
		for _, kind := range []repo.Kind{repo.Proxy, repo.Group} {
			name := f + "-" + string(kind)
			r := repo.Repository{Name: name, Format: f, Kind: kind, Enabled: true}
			if kind == repo.Proxy {
				r.Upstream = "https://upstream.invalid"
			} else {
				hosted := f + "-hosted"
				mgr.Add(repo.Repository{Name: hosted, Format: f, Kind: repo.Hosted, Enabled: true}) //nolint:errcheck
				r.Members = []string{hosted}
			}
			if err := mgr.Add(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	srv := New(mgr, reg, b, m, nil)

	for f, routes := range writes {
		for _, kind := range []repo.Kind{repo.Proxy, repo.Group} {
			repoName := f + "-" + string(kind)
			for _, route := range routes {
				method, sub := route[0], route[1]
				path := "/repository/" + repoName + "/" + sub
				rw := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, strings.NewReader("{}"))
				srv.Routes().ServeHTTP(rw, req)
				if rw.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 405 (a %s repository must refuse writes): %s",
						method, path, rw.Code, kind, strings.TrimSpace(rw.Body.String()))
				}
			}
		}
	}
}
