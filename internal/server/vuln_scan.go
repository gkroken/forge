package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"forge/internal/format"
	"forge/internal/queue"
	"forge/internal/vuln"
)

// vulnScanJobType is the async job that runs an OSV scan of one repository. It
// is registered on the shared worker (see WithQueue), mirroring webhook.JobType.
const vulnScanJobType = "vuln.scan"

type vulnScanPayload struct {
	Repo string `json:"repo"`
}

// enqueueVulnScan schedules an async OSV scan of a repository. It runs off the
// request path and is failure-isolated — a scan is never allowed to block or
// fail a publish. No-op when vuln scanning isn't configured.
func (s *Server) enqueueVulnScan(repoName string) {
	if s.Vuln == nil || s.OSV == nil || s.Queue == nil || repoName == "" {
		return
	}
	go func() {
		if err := s.Queue.Enqueue(context.Background(), vulnScanJobType, vulnScanPayload{Repo: repoName}); err != nil {
			slog.Warn("vuln: enqueue scan failed", "repo", repoName, "err", err)
		}
	}()
}

// vulnRescanInterval is how often a scannable repo is automatically re-scanned.
// Advisories are published retroactively, so a clean scan today can surface a
// finding tomorrow without any publish — periodic re-scan is the safety net.
const vulnRescanInterval = 24 * time.Hour

// vulnRescanKey namespaces a repo's re-scan timestamp in the scheduler's shared
// lastRun map so it never collides with cleanup's plain per-repo entries.
func vulnRescanKey(repo string) string { return "__vuln__:" + repo }

// trivyRescanKey namespaces an OCI repo's Trivy re-scan timestamp separately
// from the OSV keys so they never collide in the shared lastRun map.
func trivyRescanKey(repo string) string { return "__trivy__:" + repo }

// VulnRescanTick is the scheduler tick hook (registered via WithTickHook). It
// runs inside the cleanup leader lock with the shared, persisted lastRun map, so
// it fires exactly once across replicas and remembers the last re-scan across
// leadership changes. It covers two scan paths:
//   - OSV (npm, Maven): enqueues vuln.scan when VulnCoordinates is implemented.
//   - Trivy (OCI): enqueues trivy.scan.oci.repo when Trivy is configured.
func (s *Server) VulnRescanTick(now time.Time, lastRun map[string]time.Time) {
	if s.Vuln == nil || s.Queue == nil {
		return // base requirements; individual paths check their own scanner below
	}
	for _, rp := range s.Repos.All() {
		h, ok := s.Handlers.For(rp.Format)
		if !ok {
			continue
		}

		// OSV path: formats that implement VulnCoordinates (npm, Maven).
		if s.OSV != nil {
			if _, scannable := h.(format.VulnCoordinates); scannable {
				key := vulnRescanKey(rp.Name)
				if now.Sub(lastRun[key]) >= vulnRescanInterval {
					if err := s.Queue.Enqueue(context.Background(), vulnScanJobType, vulnScanPayload{Repo: rp.Name}); err != nil {
						slog.Warn("vuln: enqueue daily re-scan failed", "repo", rp.Name, "err", err)
					} else {
						lastRun[key] = now
					}
				}
			}
		}

		// Trivy path: OCI repos when the Trivy sidecar is configured.
		if s.Trivy != nil && rp.Format == "oci" {
			key := trivyRescanKey(rp.Name)
			if now.Sub(lastRun[key]) >= vulnRescanInterval {
				if err := s.Queue.Enqueue(context.Background(), trivyRepoScanJobType, trivyRepoScanPayload{Repo: rp.Name}); err != nil {
					slog.Warn("trivy: enqueue daily re-scan failed", "repo", rp.Name, "err", err)
				} else {
					lastRun[key] = now
				}
			}
		}
	}
}

// handleVulnScanJob is the worker handler for vuln.scan jobs. On a hard egress
// failure it logs and returns nil rather than erroring: a returned error would
// make the PG queue retry immediately and hammer OSV, and the scheduled re-scan
// (A-V1) is the eventual-coverage safety net. Best-effort by design.
func (s *Server) handleVulnScanJob(ctx context.Context, j queue.Job) error {
	var p vulnScanPayload
	if err := j.UnmarshalPayload(&p); err != nil {
		slog.Warn("vuln: bad scan payload", "err", err)
		return nil
	}
	if err := s.scanRepo(ctx, p.Repo); err != nil {
		slog.Warn("vuln: scan failed", "repo", p.Repo, "err", err)
	}
	return nil
}

// scanRepo enumerates a repository's components, batch-queries OSV for every
// component@version that maps to a supported ecosystem, and writes one Finding
// per version (including clean results — a Finding with no advisories records
// "scanned, no known issues" and its ScannedAt, distinguishing it from "never
// scanned"). Formats without an OSV coordinate mapping (helm/oci/cran) are
// skipped.
func (s *Server) scanRepo(ctx context.Context, repoName string) error {
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		return fmt.Errorf("repository not found: %s", repoName)
	}
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		return nil
	}
	mapper, ok := h.(format.VulnCoordinates)
	if !ok {
		return nil // format has no OSV mapping — nothing scannable
	}
	browser, ok := h.(format.Browsable)
	if !ok {
		return nil
	}

	c := &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
		Repos: s.Repos, Metrics: s.Metrics,
	}
	entries, err := browser.BrowseRepo(c)
	if err != nil {
		return err
	}

	// One coordinate per component@version that maps; slots preserve the forge
	// component/version to write findings back against.
	type slot struct{ component, version string }
	var coords []vuln.Coordinate
	var slots []slot
	for _, e := range entries {
		eco, name, ok := mapper.OSVCoordinates(e.Name)
		if !ok {
			continue
		}
		for _, v := range e.Versions {
			coords = append(coords, vuln.Coordinate{Ecosystem: eco, Name: name, Version: v})
			slots = append(slots, slot{e.Name, v})
		}
	}
	if len(coords) == 0 {
		return nil
	}

	results, err := s.OSV.Query(ctx, coords)
	if err != nil {
		return err // egress failure → handler logs and returns nil (best-effort)
	}

	now := time.Now().UTC()
	findings := make([]vuln.Finding, 0, len(results))
	for i, advs := range results {
		f := vuln.Finding{
			Component:  slots[i].component,
			Version:    slots[i].version,
			Source:     vuln.SourceOSV,
			Advisories: advs,
			ScannedAt:  now,
		}
		findings = append(findings, f)
		if err := s.Vuln.Put(repoName, f); err != nil {
			slog.Warn("vuln: store finding failed",
				"repo", repoName, "component", f.Component, "version", f.Version, "err", err)
		}
	}

	// Persist the per-repo rollup from the findings we just wrote (no re-read),
	// then refresh the gauge from it. List/aggregate surfaces read this O(1).
	rollup := vuln.BuildRollup(repoName, findings)
	if err := s.Vuln.PutRollup(repoName, rollup); err != nil {
		slog.Warn("vuln: store rollup failed", "repo", repoName, "err", err)
	}
	s.Metrics.SetVulnerableComponents(repoName, rollup.BySeverity)
	return nil
}

// handleVulnScan serves POST /api/v1/repos/{repo}/scan — enqueue an async scan
// ("Scan now"). Admin-only (gated by the caller). Returns 202 Accepted.
// Dispatches to Trivy for OCI repos and to OSV for all other scannable formats.
func (s *Server) handleVulnScan(w http.ResponseWriter, r *http.Request, repoName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Vuln == nil || s.Queue == nil {
		jsonError(w, "vulnerability scanning not configured", http.StatusServiceUnavailable)
		return
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		jsonError(w, "repository not found: "+repoName, http.StatusNotFound)
		return
	}

	// OCI repos: use the Trivy producer (not OSV).
	if rp.Format == "oci" {
		if s.Trivy == nil {
			jsonError(w, "OCI image scanning not configured (set -trivy-addr)", http.StatusNotImplemented)
			return
		}
		if err := s.Queue.Enqueue(r.Context(), trivyRepoScanJobType, trivyRepoScanPayload{Repo: repoName}); err != nil {
			jsonError(w, "enqueue failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "scan enqueued", "repo": repoName})
		return
	}

	// All other formats: OSV producer.
	if s.OSV == nil {
		jsonError(w, "vulnerability scanning not configured", http.StatusServiceUnavailable)
		return
	}
	if h, ok := s.Handlers.For(rp.Format); ok {
		if _, scannable := h.(format.VulnCoordinates); !scannable {
			jsonError(w, "format not scannable: "+rp.Format, http.StatusNotImplemented)
			return
		}
	}
	if err := s.Queue.Enqueue(r.Context(), vulnScanJobType, vulnScanPayload{Repo: repoName}); err != nil {
		jsonError(w, "enqueue failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "scan enqueued", "repo": repoName})
}
