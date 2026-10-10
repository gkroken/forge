package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/cranplatform"
	"forge/internal/format/cran"
	"forge/internal/repo"
)

type ppmStatus struct {
	Distros []map[string]any `json:"distros"`
	RVers   []string         `json:"r_versions"`
}

func getPPMStatus(t *testing.T, s *Server) ppmStatus {
	t.Helper()
	rw := httptest.NewRecorder()
	s.Routes().ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/repository/__api__/status", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rw.Code, rw.Body)
	}
	var st ppmStatus
	if err := json.Unmarshal(rw.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// renv reads distros[].binaryURL and .binaries; pak's parser reads name, os,
// binaryURL, distribution and release as strings and fails on null.
func TestPPMStatus_ListsRegisteredDistros(t *testing.T) {
	s, _ := newAuthServer(t) // public even with auth on: renv probes anonymously
	s.Handlers.Register(cran.New())
	if st := getPPMStatus(t, s); len(st.Distros) != 0 || len(st.RVers) != 0 {
		t.Fatalf("no registrations, but status lists %+v", st)
	}
	reg := cranplatform.NewStore(s.Meta)
	for _, k := range []string{"noble-x86_64/4.6", "noble-aarch64/4.6", "jammy-x86_64/4.5"} {
		p := cranplatform.Platform{Key: k, RPlatform: strings.SplitN(strings.SplitN(k, "-", 2)[1], "/", 2)[0] + "-pc-linux-gnu",
			BuilderImage: "img", BuilderDigest: linuxDigest, MaxGlibc: "2.39", Libraries: []string{"libc.so.6"}}
		if err := reg.Put(p); err != nil {
			t.Fatal(err)
		}
	}
	st := getPPMStatus(t, s)
	if len(st.Distros) != 2 || st.Distros[0]["binaryURL"] != "jammy" || st.Distros[1]["binaryURL"] != "noble" {
		t.Fatalf("distros: %+v", st.Distros)
	}
	for _, d := range st.Distros {
		if d["binaries"] != true {
			t.Errorf("%v: binaries not true", d["name"])
		}
		for _, f := range []string{"name", "os", "binaryURL", "distribution", "release"} {
			if _, ok := d[f].(string); !ok {
				t.Errorf("%v: field %s is not a string (pak would fail)", d["name"], f)
			}
		}
	}
	if strings.Join(st.RVers, ",") != "4.6,4.5" {
		t.Errorf("r_versions: %v", st.RVers)
	}
}

func TestRepoNames_DoubleUnderscoreReserved(t *testing.T) {
	m := repo.NewManager()
	if err := m.Add(repo.Repository{Name: "__api__", Format: "cran", Kind: repo.Hosted}); err == nil {
		t.Fatal("a repository named __api__ would be shadowed by forge's status endpoint")
	}
}
