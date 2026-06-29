package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"forge/internal/format"
	"forge/internal/queue"
	"forge/internal/trivy"
	"forge/internal/vuln"
)

// trivyScanJobType is the async job that runs a Trivy scan of one OCI image
// tag. Registered on the shared worker (see WithQueue) when Trivy is configured,
// mirroring vulnScanJobType for OSV.
const trivyScanJobType = "trivy.scan.oci"

type trivyScanPayload struct {
	Repo  string `json:"repo"`
	Image string `json:"image"`
	Tag   string `json:"tag"`
}

// enqueueTrivyScan schedules an async Trivy scan of one image tag. Runs off the
// request path and is failure-isolated — a scan is never allowed to block or
// fail a manifest push. No-op when Trivy isn't configured.
func (s *Server) enqueueTrivyScan(repoName, image, tag string) {
	if s.Trivy == nil || s.Vuln == nil || s.Queue == nil {
		return
	}
	if repoName == "" || image == "" || tag == "" {
		return
	}
	go func() {
		p := trivyScanPayload{Repo: repoName, Image: image, Tag: tag}
		if err := s.Queue.Enqueue(context.Background(), trivyScanJobType, p); err != nil {
			slog.Warn("trivy: enqueue scan failed",
				"repo", repoName, "image", image, "tag", tag, "err", err)
		}
	}()
}

// handleTrivyScanJob is the worker handler for trivy.scan.oci jobs. Returns nil
// on scan failure so the PG queue can't retry-spin and hammer Trivy; the periodic
// re-scan (B-O1) is the eventual-coverage safety net.
func (s *Server) handleTrivyScanJob(ctx context.Context, j queue.Job) error {
	var p trivyScanPayload
	if err := j.UnmarshalPayload(&p); err != nil {
		slog.Warn("trivy: bad scan payload", "err", err)
		return nil
	}
	if err := s.scanOCIImage(ctx, p.Repo, p.Image, p.Tag); err != nil {
		slog.Warn("trivy: scan failed",
			"repo", p.Repo, "image", p.Image, "tag", p.Tag, "err", err)
	}
	return nil
}

// scanOCIImage runs Trivy against one image tag in a forge OCI repository and
// writes the resulting Finding (including clean results — a Finding with no
// advisories records "scanned, no known vulnerabilities" + ScannedAt).
//
// Unlike the OSV scanner (which batch-scans a whole repo at once), Trivy scans
// one image/tag at a time. The rollup is therefore rebuilt from all current
// findings in the repo rather than from the in-memory scan batch.
func (s *Server) scanOCIImage(ctx context.Context, repoName, image, tag string) error {
	if s.Trivy == nil || s.Vuln == nil {
		return nil
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		return fmt.Errorf("trivy: repository not found: %s", repoName)
	}
	if rp.Format != "oci" {
		return fmt.Errorf("trivy: not an OCI repository: %s (format=%s)", repoName, rp.Format)
	}

	ref := s.Trivy.ImageRef(repoName, image, tag)
	slog.Info("trivy: scanning image", "repo", repoName, "image", image, "tag", tag, "ref", ref)

	advs, err := s.Trivy.ScanImage(ctx, ref)
	if err != nil {
		return err
	}

	f := vuln.Finding{
		Component:  image,
		Version:    tag,
		Source:     vuln.SourceTrivy,
		Advisories: advs,
		ScannedAt:  time.Now().UTC(),
	}
	if err := s.Vuln.Put(repoName, f); err != nil {
		slog.Warn("trivy: store finding failed",
			"repo", repoName, "image", image, "tag", tag, "err", err)
	}

	// Rebuild the rollup from all findings in this repo. Trivy scans one tag at a
	// time, so we must aggregate from the store rather than from the in-memory
	// batch (which is a single Finding). A storage error here is non-fatal.
	findings, err := s.Vuln.List(repoName)
	if err == nil {
		rollup := vuln.BuildRollup(repoName, findings)
		if err := s.Vuln.PutRollup(repoName, rollup); err != nil {
			slog.Warn("trivy: store rollup failed", "repo", repoName, "err", err)
		}
		if s.Metrics != nil {
			s.Metrics.SetVulnerableComponents(repoName, rollup.BySeverity)
		}
	} else {
		slog.Warn("trivy: list findings for rollup failed", "repo", repoName, "err", err)
	}

	if len(advs) > 0 {
		slog.Info("trivy: scan complete",
			"repo", repoName, "image", image, "tag", tag, "advisories", len(advs))
	} else {
		slog.Info("trivy: scan complete — no known vulnerabilities",
			"repo", repoName, "image", image, "tag", tag)
	}
	return nil
}

// trivyScannable reports whether the OCI handler for a repo implements the Trivy
// scanning path (format == "oci" and Trivy is configured). Used to extend the
// daily re-scan tick (B-O1) and manual scan endpoint to cover OCI.
func (s *Server) trivyScannable(format string) bool {
	return s.Trivy != nil && s.Vuln != nil && format == "oci"
}

// WithTrivy attaches the Trivy sidecar scanner. Call before WithQueue (which
// registers the scan handler) and before Routes() (so the manifest-push hook
// enqueues scans). nil leaves OCI scanning disabled.
func (s *Server) WithTrivy(sc *trivy.Scanner) *Server {
	s.Trivy = sc
	return s
}

// scanOCIRepo enumerates all images+tags in an OCI repository and scans each
// via Trivy. Used by the manual /scan endpoint (B-O1) and the daily re-scan
// tick (B-O1). Returns nil when Trivy isn't configured or the repo has no tags.
func (s *Server) scanOCIRepo(ctx context.Context, repoName string) error {
	h, ok := s.Handlers.For("oci")
	if !ok {
		return nil
	}
	browser, ok := h.(format.Browsable)
	if !ok {
		return nil
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		return fmt.Errorf("trivy: repository not found: %s", repoName)
	}
	c := &format.Context{
		Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
		Repos: s.Repos, Metrics: s.Metrics,
	}
	entries, err := browser.BrowseRepo(c)
	if err != nil {
		return err
	}
	for _, e := range entries {
		for _, tag := range e.Versions {
			if err := s.scanOCIImage(ctx, repoName, e.Name, tag); err != nil {
				slog.Warn("trivy: scan image failed during repo scan",
					"repo", repoName, "image", e.Name, "tag", tag, "err", err)
				// continue — partial scan beats no scan
			}
		}
	}
	return nil
}
