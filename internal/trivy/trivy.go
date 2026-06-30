// Package trivy wraps the Trivy binary as a vulnerability-scanning sidecar for
// OCI images hosted in forge. It is the only approved way to add OCI scanning
// while staying stdlib-only: Trivy (and Grype) are large Go modules with their
// own vuln-DB download logic, so they run as an external binary, not an import.
//
// The Scanner calls `trivy image --format json --quiet --insecure {ref}` and
// parses the JSON output into []vuln.Advisory. Findings flow into the same
// vuln.Store / Security UI / policy gate as Plan A (OSV) — the source-agnostic
// spine is what makes this a drop-in producer.
package trivy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"forge/internal/vuln"
)

// Executor runs an external command with optional environment overrides and
// returns its combined stdout. The abstraction lets tests inject a fake without
// spawning a real Trivy process.
type Executor interface {
	Run(ctx context.Context, env []string, args ...string) ([]byte, error)
}

// osExecutor is the real Executor that delegates to os/exec.
type osExecutor struct{ binary string }

func (e *osExecutor) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// cmd.Output() returns (stdout, err). When the process exits non-zero (e.g.
	// trivy --exit-code 1 found vulns), stdout still contains the JSON report.
	// We preserve the output and let the caller decide whether the error matters.
	out, err := cmd.Output()
	return out, err
}

// Scanner wraps a Trivy binary, pointing it at forge's own OCI registry to scan
// images that forge already hosts. It is safe to use concurrently.
type Scanner struct {
	exec         Executor
	registryAddr string // e.g. "localhost:8080"
	authToken    string // forge API token → TRIVY_REGISTRY_TOKEN; empty = no auth
}

// New returns a Scanner. binary is the path to the Trivy executable (looked up
// in PATH when empty or "trivy"). registryAddr is the forge host:port that Trivy
// uses when pulling images (e.g. "localhost:8080"). authToken is an optional
// forge API token; when non-empty it is forwarded as TRIVY_REGISTRY_TOKEN so
// Trivy can authenticate against an auth-enabled forge registry.
func New(binary, registryAddr, authToken string) *Scanner {
	if binary == "" {
		binary = "trivy"
	}
	return &Scanner{
		exec:         &osExecutor{binary: binary},
		registryAddr: strings.TrimRight(registryAddr, "/"),
		authToken:    authToken,
	}
}

// WithExecutor replaces the executor and returns the receiver. Used in tests to
// inject a fake without spawning a real Trivy process.
func (s *Scanner) WithExecutor(e Executor) *Scanner {
	s.exec = e
	return s
}

// ImageRef builds the full image reference Trivy uses to pull from forge.
// The forge OCI registry is mounted at /v2/{repoName}/, so an OCI client sees
// the image as: {registryAddr}/{repoName}/{image}:{tag}
func (s *Scanner) ImageRef(repoName, image, tag string) string {
	return s.registryAddr + "/" + repoName + "/" + image + ":" + tag
}

// ScanImage runs Trivy against ref (a fully-qualified image reference such as
// "localhost:8080/docker-hosted/myapp:latest") and returns the advisories found
// across all image layers. An empty slice with a nil error means Trivy ran
// successfully and found no known vulnerabilities. Vulnerabilities that appear in
// multiple layers are deduplicated by VulnerabilityID.
func (s *Scanner) ScanImage(ctx context.Context, ref string) ([]vuln.Advisory, error) {
	args := []string{
		"image",
		"--format", "json",
		"--quiet",
		"--insecure", // forge uses HTTP in eval; TLS in prod where --insecure is a no-op
		ref,
	}
	var env []string
	if s.authToken != "" {
		env = []string{"TRIVY_REGISTRY_TOKEN=" + s.authToken}
	}

	out, execErr := s.exec.Run(ctx, env, args...)
	// Parse whatever output we got first; if parsing succeeds, the exec error is
	// a non-fatal scan signal (e.g. trivy --exit-code 1 found vulnerabilities).
	if len(out) > 0 {
		advs, parseErr := parseOutput(out)
		if parseErr == nil {
			return advs, nil
		}
	}
	if execErr != nil {
		return nil, fmt.Errorf("trivy: scan %s: %w", ref, execErr)
	}
	return nil, fmt.Errorf("trivy: scan %s: empty output", ref)
}

// ScanConfigFile runs Trivy's misconfiguration scanner (`trivy config`) against
// a local path — a Helm chart .tgz or an unpacked chart directory; Trivy renders
// the templates and checks the resulting K8s manifests. It returns the failing
// checks as advisories tagged by the caller with vuln.SourceTrivyConfig. This is
// static IaC analysis, not a CVE-DB lookup. An empty slice with a nil error means
// Trivy ran and found no failing checks. No registry/auth is involved (it reads a
// local file), so no TRIVY_REGISTRY_TOKEN is passed.
func (s *Scanner) ScanConfigFile(ctx context.Context, path string) ([]vuln.Advisory, error) {
	args := []string{"config", "--format", "json", "--quiet", path}
	out, execErr := s.exec.Run(ctx, nil, args...)
	if len(out) > 0 {
		advs, parseErr := parseConfigOutput(out)
		if parseErr == nil {
			return advs, nil
		}
	}
	if execErr != nil {
		return nil, fmt.Errorf("trivy: config scan %s: %w", path, execErr)
	}
	return nil, fmt.Errorf("trivy: config scan %s: empty output", path)
}

// parseConfigOutput converts a `trivy config` JSON report to []vuln.Advisory.
// The same misconfiguration rule can fail on several resources in one chart; we
// deduplicate by rule ID (a chart-level "this rule fails" signal — severity for
// a given ID is fixed). Only FAIL-status entries are kept (trivy config emits
// failures by default, but guard in case PASS slips in).
func parseConfigOutput(data []byte) ([]vuln.Advisory, error) {
	var report trivyReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("trivy: parse config output: %w", err)
	}
	seen := map[string]bool{}
	var out []vuln.Advisory
	for _, target := range report.Results {
		for _, mc := range target.Misconfigurations {
			if mc.ID == "" {
				continue
			}
			if mc.Status != "" && strings.ToUpper(mc.Status) != "FAIL" {
				continue
			}
			if seen[mc.ID] {
				continue
			}
			seen[mc.ID] = true
			url := mc.PrimaryURL
			if url == "" && len(mc.References) > 0 {
				url = mc.References[0]
			}
			out = append(out, vuln.Advisory{
				ID:       mc.ID,
				Summary:  mc.Title,
				Severity: mapSeverity(mc.Severity),
				URL:      url,
			})
		}
	}
	return out, nil
}

// --- JSON model (trivy image --format json) ----------------------------------

type trivyReport struct {
	Results []trivyTarget `json:"Results"`
}

type trivyTarget struct {
	Target            string           `json:"Target"`
	Vulnerabilities   []trivyVuln      `json:"Vulnerabilities"`
	Misconfigurations []trivyMisconfig `json:"Misconfigurations"`
}

// trivyMisconfig is one entry from `trivy config` (Results[].Misconfigurations[]).
// Distinct schema from trivyVuln: no installed/fixed version (it's a static IaC
// finding, not a package CVE), and the canonical link is PrimaryURL.
type trivyMisconfig struct {
	ID         string   `json:"ID"` // e.g. "KSV-0001"
	Title      string   `json:"Title"`
	Severity   string   `json:"Severity"`
	Resolution string   `json:"Resolution"`
	PrimaryURL string   `json:"PrimaryURL"`
	References []string `json:"References"`
	Status     string   `json:"Status"` // "FAIL" for actual findings
}

type trivyVuln struct {
	VulnerabilityID  string   `json:"VulnerabilityID"`
	Title            string   `json:"Title"`
	Severity         string   `json:"Severity"`
	InstalledVersion string   `json:"InstalledVersion"`
	FixedVersion     string   `json:"FixedVersion"`
	References       []string `json:"References"`
}

// parseOutput converts Trivy's JSON report to []vuln.Advisory. Vulnerabilities
// that appear in multiple layers (e.g. the same CVE in both the OS layer and a
// bundled lib) are deduplicated by VulnerabilityID — the worst severity wins.
func parseOutput(data []byte) ([]vuln.Advisory, error) {
	var report trivyReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("trivy: parse output: %w", err)
	}

	seen := map[string]vuln.Advisory{}
	order := []string{} // preserve first-seen order for deterministic output
	for _, target := range report.Results {
		for _, v := range target.Vulnerabilities {
			if v.VulnerabilityID == "" {
				continue
			}
			sev := mapSeverity(v.Severity)
			url := ""
			if len(v.References) > 0 {
				url = v.References[0]
			}
			var fixedIn []string
			if v.FixedVersion != "" {
				fixedIn = []string{v.FixedVersion}
			}
			if prev, exists := seen[v.VulnerabilityID]; exists {
				// Keep worst severity across layers.
				if sev > prev.Severity {
					prev.Severity = sev
					seen[v.VulnerabilityID] = prev
				}
				continue
			}
			a := vuln.Advisory{
				ID:      v.VulnerabilityID,
				Summary: v.Title,
				Severity: sev,
				FixedIn: fixedIn,
				URL:     url,
			}
			seen[v.VulnerabilityID] = a
			order = append(order, v.VulnerabilityID)
		}
	}

	out := make([]vuln.Advisory, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	return out, nil
}

// mapSeverity converts Trivy's severity label to our ordered bucket.
func mapSeverity(s string) vuln.Severity {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CRITICAL":
		return vuln.SeverityCritical
	case "HIGH":
		return vuln.SeverityHigh
	case "MEDIUM":
		return vuln.SeverityModerate
	case "LOW":
		return vuln.SeverityLow
	default:
		return vuln.SeverityUnknown
	}
}
