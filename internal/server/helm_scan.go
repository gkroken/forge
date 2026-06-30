package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"forge/internal/format"
	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/vuln"
)

// helmRepoScanJobType is the async job that runs a Trivy config (misconfiguration)
// scan of every chart in a Helm repository. Unlike OCI — where a manifest push
// carries the image+tag — a chart upload's name/version live inside the .tgz body,
// not the request path, so the on-publish hook can only enqueue a whole-repo scan
// (mirroring the OSV whole-repo granularity). Registered on the shared worker when
// Trivy is configured.
const helmRepoScanJobType = "helm.scan.config"

type helmRepoScanPayload struct {
	Repo string `json:"repo"`
}

// helmScannable reports whether a repo's charts can be config-scanned: the Trivy
// sidecar is configured and the format is helm. Used by the on-publish enqueue,
// the manual scan endpoint, the daily re-scan tick, and the detail-pane state.
func (s *Server) helmScannable(format string) bool {
	return s.Trivy != nil && s.Vuln != nil && format == "helm"
}

// enqueueHelmScan schedules an async Trivy config scan of a Helm repository. Runs
// off the request path and is failure-isolated — never blocks or fails a chart
// upload. No-op unless Trivy is configured and the repo is a helm repo.
func (s *Server) enqueueHelmScan(repoName string) {
	if s.Queue == nil || repoName == "" {
		return
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok || !s.helmScannable(rp.Format) {
		return
	}
	go func() {
		if err := s.Queue.Enqueue(context.Background(), helmRepoScanJobType, helmRepoScanPayload{Repo: repoName}); err != nil {
			slog.Warn("helm: enqueue config scan failed", "repo", repoName, "err", err)
		}
	}()
}

// handleHelmRepoScanJob is the worker handler for helm.scan.config jobs. Returns
// nil on failure so the PG queue can't retry-spin; the daily re-scan is the
// safety net.
func (s *Server) handleHelmRepoScanJob(ctx context.Context, j queue.Job) error {
	var p helmRepoScanPayload
	if err := j.UnmarshalPayload(&p); err != nil {
		slog.Warn("helm: bad scan payload", "err", err)
		return nil
	}
	if err := s.scanHelmRepo(ctx, p.Repo); err != nil {
		slog.Warn("helm: config scan failed", "repo", p.Repo, "err", err)
	}
	return nil
}

// scanReferencedImages scans the external images a chart references (from its
// values.yaml) and returns their advisories, each summary prefixed with the image
// ref. Format-agnostic: it asks the helm handler for the refs via the optional
// format.ReferencedImages seam, then reuses the Plan B external-image scanner.
// Returns nil (never errors) — referenced-image scanning supplements the config
// scan and must never fail the chart scan.
func (s *Server) scanReferencedImages(ctx context.Context, rp repo.Repository, chart, version string) []vuln.Advisory {
	h, ok := s.Handlers.For(rp.Format)
	if !ok {
		return nil
	}
	ri, ok := h.(format.ReferencedImages)
	if !ok {
		return nil
	}
	c := &format.Context{Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client, Repos: s.Repos}
	refs, err := ri.ReferencedImages(c, chart, version)
	if err != nil {
		slog.Debug("helm: no referenced images", "repo", rp.Name, "chart", chart, "version", version, "err", err)
		return nil
	}
	var out []vuln.Advisory
	for _, ref := range refs {
		imgAdvs, err := s.Trivy.ScanExternalImage(ctx, ref)
		if err != nil {
			slog.Warn("helm: referenced image scan failed",
				"repo", rp.Name, "chart", chart, "ref", ref, "err", err)
			continue
		}
		for _, a := range imgAdvs {
			if a.Summary != "" {
				a.Summary = ref + ": " + a.Summary
			} else {
				a.Summary = ref
			}
			out = append(out, a)
		}
		slog.Info("helm: scanned referenced image",
			"repo", rp.Name, "chart", chart, "ref", ref, "advisories", len(imgAdvs))
	}
	return out
}

// scanHelmRepo enumerates a Helm repository's charts and runs a Trivy config scan
// on each chart version. Returns nil when Trivy isn't configured or the repo has
// no charts.
func (s *Server) scanHelmRepo(ctx context.Context, repoName string) error {
	if s.Trivy == nil || s.Vuln == nil {
		return nil
	}
	h, ok := s.Handlers.For("helm")
	if !ok {
		return nil
	}
	browser, ok := h.(format.Browsable)
	if !ok {
		return nil
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		return fmt.Errorf("helm: repository not found: %s", repoName)
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
		for _, ver := range e.Versions {
			if err := s.scanHelmChart(ctx, repoName, e.Name, ver); err != nil {
				slog.Warn("helm: scan chart failed during repo scan",
					"repo", repoName, "chart", e.Name, "version", ver, "err", err)
				// continue — partial scan beats no scan
			}
		}
	}
	return nil
}

// scanHelmChart runs Trivy's config scanner against one stored chart version and
// writes the resulting Finding (including clean results — an empty advisory list
// records "scanned, no misconfigurations" + ScannedAt). The chart .tgz is copied
// from the blob store to a temp file because `trivy config` reads a path, not a
// stream. The rollup is rebuilt from all current findings in the repo.
func (s *Server) scanHelmChart(ctx context.Context, repoName, chart, version string) error {
	if s.Trivy == nil || s.Vuln == nil {
		return nil
	}
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		return fmt.Errorf("helm: repository not found: %s", repoName)
	}
	if rp.Format != "helm" {
		return fmt.Errorf("helm: not a Helm repository: %s (format=%s)", repoName, rp.Format)
	}

	// Helm stores each chart as {repo}/{chart}-{version}.tgz (see helm.upload).
	blobKey := repoName + "/" + chart + "-" + version + ".tgz"
	rc, err := s.Blob.Get(blobKey)
	if err != nil {
		return fmt.Errorf("helm: read chart %s: %w", blobKey, err)
	}
	tmp, err := os.CreateTemp("", "forge-chart-*.tgz")
	if err != nil {
		rc.Close()
		return fmt.Errorf("helm: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_, copyErr := io.Copy(tmp, rc)
	rc.Close()
	tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("helm: buffer chart %s: %w", blobKey, copyErr)
	}

	slog.Info("helm: scanning chart config", "repo", repoName, "chart", chart, "version", version)
	advs, err := s.Trivy.ScanConfigFile(ctx, tmpPath)
	if err != nil {
		return err
	}

	// C-2: also scan images the chart references in values.yaml. These usually
	// live in external registries forge doesn't host, so Trivy pulls them from
	// upstream. Merged into the same chart Finding (the store is one finding per
	// component@version); each image-CVE summary is prefixed with its ref for
	// traceability, and the CVE-*/KSV-* ID convention distinguishes the classes.
	// Best-effort: an unreachable/private image is logged and skipped.
	advs = append(advs, s.scanReferencedImages(ctx, rp, chart, version)...)

	f := vuln.Finding{
		Component:  chart,
		Version:    version,
		Source:     vuln.SourceTrivyConfig,
		Advisories: advs,
		ScannedAt:  time.Now().UTC(),
	}
	if err := s.Vuln.Put(repoName, f); err != nil {
		slog.Warn("helm: store finding failed",
			"repo", repoName, "chart", chart, "version", version, "err", err)
	}

	// Rebuild the rollup from all findings in this repo (Trivy scans one chart at
	// a time, so aggregate from the store, not the single in-memory Finding).
	findings, err := s.Vuln.List(repoName)
	if err == nil {
		rollup := vuln.BuildRollup(repoName, findings)
		if err := s.Vuln.PutRollup(repoName, rollup); err != nil {
			slog.Warn("helm: store rollup failed", "repo", repoName, "err", err)
		}
		if s.Metrics != nil {
			s.Metrics.SetVulnerableComponents(repoName, rollup.BySeverity)
		}
	} else {
		slog.Warn("helm: list findings for rollup failed", "repo", repoName, "err", err)
	}

	if len(advs) > 0 {
		slog.Info("helm: config scan complete",
			"repo", repoName, "chart", chart, "version", version, "misconfigurations", len(advs))
	} else {
		slog.Info("helm: config scan complete — no misconfigurations",
			"repo", repoName, "chart", chart, "version", version)
	}
	return nil
}
