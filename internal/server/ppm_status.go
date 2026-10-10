package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"forge/internal/format"
)

// handlePPMStatus serves GET /repository/__api__/status in the shape of Posit
// Package Manager's status endpoint, for renv.
//
// renv records a PPM-shaped repository in lockfiles in its source form,
// {repo}/latest, and at restore asks dirname(dirname(url)) + "/__api__/status"
// — /repository/__api__/status for a forge URL — whether binaries exist for
// the client's distro; only then does it use {repo}/__linux__/{distro}/latest
// (measured: docs/notes/cran-linux-clients.md §2). The routing stays
// format-agnostic: formats that serve such binaries answer through
// format.PPMStatusProvider.
//
// Public: renv probes without credentials, and the answer names only distros
// and R versions.
func (s *Server) handlePPMStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type distro struct {
		Name          string `json:"name"`
		OS            string `json:"os"`
		BinaryURL     string `json:"binaryURL"`
		Display       string `json:"display"`
		BinaryDisplay string `json:"binaryDisplay"`
		// pak reads these as strings (vcapply) and fails on null; forge does
		// not record them, so they are empty rather than absent.
		Distribution string `json:"distribution"`
		Release      string `json:"release"`
		Binaries     bool   `json:"binaries"`
	}
	distros := map[string]bool{}
	rvers := map[string]bool{}
	for _, f := range s.Handlers.Formats() {
		h, _ := s.Handlers.For(f)
		if p, ok := h.(format.PPMStatusProvider); ok {
			ds, rs := p.PPMStatus(s.Meta)
			for _, d := range ds {
				distros[d] = true
			}
			for _, v := range rs {
				rvers[v] = true
			}
		}
	}
	out := struct {
		Distros   []distro `json:"distros"`
		RVersions []string `json:"r_versions"`
	}{Distros: []distro{}, RVersions: []string{}}
	for d := range distros {
		out.Distros = append(out.Distros, distro{Name: d, OS: "linux", BinaryURL: d, Display: d, BinaryDisplay: d, Binaries: true})
	}
	sort.Slice(out.Distros, func(i, j int) bool { return out.Distros[i].Name < out.Distros[j].Name })
	for v := range rvers {
		out.RVersions = append(out.RVersions, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out.RVersions)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
