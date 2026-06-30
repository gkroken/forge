package trivy

import (
	"context"
	"errors"
	"testing"

	"forge/internal/vuln"
)

// fakeExecutor returns canned output without invoking the real trivy binary.
type fakeExecutor struct {
	out []byte
	err error
}

func (f *fakeExecutor) Run(_ context.Context, _ []string, _ ...string) ([]byte, error) {
	return f.out, f.err
}

func scanner(out string) *Scanner {
	return New("trivy", "localhost:8080", "").WithExecutor(&fakeExecutor{out: []byte(out)})
}

const trivyClean = `{"Results":[]}`

const trivyOneVuln = `{
  "Results": [
    {
      "Target": "alpine",
      "Vulnerabilities": [
        {
          "VulnerabilityID": "CVE-2023-1234",
          "Title": "SSL buffer overflow",
          "Severity": "HIGH",
          "InstalledVersion": "1.0.0",
          "FixedVersion": "1.0.1",
          "References": ["https://nvd.nist.gov/vuln/detail/CVE-2023-1234"]
        }
      ]
    }
  ]
}`

const trivyTwoLayers = `{
  "Results": [
    {
      "Target": "layer1",
      "Vulnerabilities": [
        {"VulnerabilityID": "CVE-2023-1234", "Severity": "HIGH", "References": ["https://example.com/1"]}
      ]
    },
    {
      "Target": "layer2",
      "Vulnerabilities": [
        {"VulnerabilityID": "CVE-2023-1234", "Severity": "CRITICAL", "References": ["https://example.com/2"]},
        {"VulnerabilityID": "CVE-2023-5678", "Severity": "LOW"}
      ]
    }
  ]
}`

func TestParseOutput_Clean(t *testing.T) {
	advs, err := parseOutput([]byte(trivyClean))
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 0 {
		t.Fatalf("want 0 advisories, got %d", len(advs))
	}
}

func TestParseOutput_SingleVuln(t *testing.T) {
	advs, err := parseOutput([]byte(trivyOneVuln))
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 1 {
		t.Fatalf("want 1 advisory, got %d", len(advs))
	}
	a := advs[0]
	if a.ID != "CVE-2023-1234" {
		t.Errorf("ID: want CVE-2023-1234, got %s", a.ID)
	}
	if a.Severity != vuln.SeverityHigh {
		t.Errorf("Severity: want High, got %v", a.Severity)
	}
	if len(a.FixedIn) != 1 || a.FixedIn[0] != "1.0.1" {
		t.Errorf("FixedIn: want [1.0.1], got %v", a.FixedIn)
	}
	if a.URL != "https://nvd.nist.gov/vuln/detail/CVE-2023-1234" {
		t.Errorf("URL: unexpected %s", a.URL)
	}
}

func TestParseOutput_DeduplicatesAcrossLayers(t *testing.T) {
	advs, err := parseOutput([]byte(trivyTwoLayers))
	if err != nil {
		t.Fatal(err)
	}
	// CVE-2023-1234 appears in both layers; worst severity (CRITICAL) wins.
	// CVE-2023-5678 appears once.
	if len(advs) != 2 {
		t.Fatalf("want 2 advisories after dedup, got %d", len(advs))
	}
	byID := map[string]vuln.Advisory{}
	for _, a := range advs {
		byID[a.ID] = a
	}
	if byID["CVE-2023-1234"].Severity != vuln.SeverityCritical {
		t.Errorf("CVE-2023-1234: want Critical (worst across layers), got %v",
			byID["CVE-2023-1234"].Severity)
	}
	if byID["CVE-2023-5678"].Severity != vuln.SeverityLow {
		t.Errorf("CVE-2023-5678: want Low, got %v", byID["CVE-2023-5678"].Severity)
	}
}

// trivyConfigReport mirrors the real `trivy config --format json` shape (captured
// from trivy v0.72.0 scanning a Helm chart): Results[].Misconfigurations[], each
// with a FAIL Status, a KSV rule ID, and PrimaryURL as the canonical link. The
// duplicate KSV-0001 (two resources fail the same rule) exercises ID dedup; the
// PASS entry must be dropped.
const trivyConfigReport = `{
  "Results": [
    {
      "Target": "templates/pod.yaml",
      "Class": "config",
      "Type": "kubernetes",
      "Misconfigurations": [
        {"ID": "KSV-0001", "Title": "Can elevate its own privileges", "Severity": "MEDIUM",
         "Status": "FAIL", "PrimaryURL": "https://avd.aquasec.com/misconfig/ksv001",
         "References": ["https://example.com/ref"]},
        {"ID": "KSV-0017", "Title": "Privileged container", "Severity": "HIGH",
         "Status": "FAIL", "PrimaryURL": "https://avd.aquasec.com/misconfig/ksv017"},
        {"ID": "KSV-0118", "Title": "Default security context configured", "Severity": "LOW",
         "Status": "FAIL", "References": ["https://example.com/only-ref"]}
      ]
    },
    {
      "Target": "templates/svc.yaml",
      "Class": "config",
      "Type": "kubernetes",
      "Misconfigurations": [
        {"ID": "KSV-0001", "Title": "Can elevate its own privileges", "Severity": "MEDIUM",
         "Status": "FAIL", "PrimaryURL": "https://avd.aquasec.com/misconfig/ksv001"},
        {"ID": "KSV-9999", "Title": "Passed check", "Severity": "HIGH", "Status": "PASS"}
      ]
    }
  ]
}`

func TestParseConfigOutput(t *testing.T) {
	advs, err := parseConfigOutput([]byte(trivyConfigReport))
	if err != nil {
		t.Fatal(err)
	}
	// KSV-0001 (deduped), KSV-0017, KSV-0118 — KSV-9999 (PASS) dropped.
	if len(advs) != 3 {
		t.Fatalf("want 3 advisories, got %d: %+v", len(advs), advs)
	}
	byID := map[string]vuln.Advisory{}
	for _, a := range advs {
		byID[a.ID] = a
	}
	if _, ok := byID["KSV-9999"]; ok {
		t.Error("PASS-status check must be excluded")
	}
	if byID["KSV-0017"].Severity != vuln.SeverityHigh {
		t.Errorf("KSV-0017 severity: want High, got %v", byID["KSV-0017"].Severity)
	}
	if byID["KSV-0001"].URL != "https://avd.aquasec.com/misconfig/ksv001" {
		t.Errorf("KSV-0001 URL: want PrimaryURL, got %q", byID["KSV-0001"].URL)
	}
	// PrimaryURL absent → fall back to References[0].
	if byID["KSV-0118"].URL != "https://example.com/only-ref" {
		t.Errorf("KSV-0118 URL: want References fallback, got %q", byID["KSV-0118"].URL)
	}
	if byID["KSV-0017"].Summary != "Privileged container" {
		t.Errorf("KSV-0017 summary: %q", byID["KSV-0017"].Summary)
	}
}

func TestScanConfigFile(t *testing.T) {
	s := scanner(trivyConfigReport)
	advs, err := s.ScanConfigFile(context.Background(), "/charts/badchart-0.1.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 3 {
		t.Fatalf("want 3 advisories, got %d", len(advs))
	}
}

func TestScanConfigFile_Clean(t *testing.T) {
	s := scanner(trivyClean)
	advs, err := s.ScanConfigFile(context.Background(), "/charts/ok.tgz")
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 0 {
		t.Fatalf("want 0 advisories for clean chart, got %d", len(advs))
	}
}

func TestScanConfigFile_ExecErrorNoOutput(t *testing.T) {
	s := New("trivy", "", "").WithExecutor(&fakeExecutor{err: errors.New("exit status 127")})
	if _, err := s.ScanConfigFile(context.Background(), "/charts/x.tgz"); err == nil {
		t.Fatal("expected error when trivy binary not found")
	}
}

func TestMapSeverity(t *testing.T) {
	cases := []struct {
		in   string
		want vuln.Severity
	}{
		{"CRITICAL", vuln.SeverityCritical},
		{"HIGH", vuln.SeverityHigh},
		{"MEDIUM", vuln.SeverityModerate},
		{"LOW", vuln.SeverityLow},
		{"UNKNOWN", vuln.SeverityUnknown},
		{"", vuln.SeverityUnknown},
	}
	for _, tc := range cases {
		if got := mapSeverity(tc.in); got != tc.want {
			t.Errorf("mapSeverity(%q): want %v, got %v", tc.in, tc.want, got)
		}
	}
}

func TestScanImage_Clean(t *testing.T) {
	s := scanner(trivyClean)
	advs, err := s.ScanImage(context.Background(), "localhost:8080/repo/img:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 0 {
		t.Fatalf("want 0 advisories for clean image, got %d", len(advs))
	}
}

func TestScanImage_WithFindings(t *testing.T) {
	s := scanner(trivyOneVuln)
	advs, err := s.ScanImage(context.Background(), "localhost:8080/repo/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 1 {
		t.Fatalf("want 1 advisory, got %d", len(advs))
	}
}

func TestScanImage_ExecErrorWithOutput(t *testing.T) {
	// Trivy exits non-zero when --exit-code 1 is set and vulns are found.
	// We should still parse the output successfully.
	s := New("trivy", "localhost:8080", "").WithExecutor(&fakeExecutor{
		out: []byte(trivyOneVuln),
		err: errors.New("exit status 1"),
	})
	advs, err := s.ScanImage(context.Background(), "localhost:8080/repo/img:v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(advs) != 1 {
		t.Fatalf("want 1 advisory even on non-zero exit, got %d", len(advs))
	}
}

func TestScanImage_ExecErrorNoOutput(t *testing.T) {
	s := New("trivy", "localhost:8080", "").WithExecutor(&fakeExecutor{
		err: errors.New("exit status 127"),
	})
	_, err := s.ScanImage(context.Background(), "localhost:8080/repo/img:v1")
	if err == nil {
		t.Fatal("expected error when trivy binary not found")
	}
}

func TestImageRef(t *testing.T) {
	s := New("trivy", "localhost:8080/", "") // trailing slash stripped
	got := s.ImageRef("docker-hosted", "myapp", "latest")
	want := "localhost:8080/docker-hosted/myapp:latest"
	if got != want {
		t.Errorf("ImageRef: want %s, got %s", want, got)
	}
}
