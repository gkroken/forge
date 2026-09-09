package helm

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Chart name and version come from Chart.yaml INSIDE the uploaded archive, so a
// publisher controls them, and they land in two places that care: a blob path
// and the generated index.yaml.
//
// A name containing ": " made the index unparseable — `helm repo add` failed
// for every client of the repository until an admin found and removed the
// chart. Description was already quoted for exactly this reason; name and
// version were not, so the earlier fix covered the field that broke rather than
// the class.

func TestValidChartName(t *testing.T) {
	for _, ok := range []string{"nginx", "my-chart", "chart.v2", "a", "Chart_1", "postgresql-ha"} {
		if !validChartName(ok) {
			t.Errorf("validChartName(%q) = false, want true — real charts must publish", ok)
		}
	}
	for _, bad := range []string{
		"", "../escape", "a/b/c", "name: injected", "outer\n  injected:", `quo"te`,
		"-leading", "trailing-", "sp ace",
	} {
		if validChartName(bad) {
			t.Errorf("validChartName(%q) = true, want false", bad)
		}
	}
}

func TestValidChartVersion(t *testing.T) {
	for _, ok := range []string{"1.0.0", "0.1.0-alpha.1", "2.0.0+build.5", "10.20.30"} {
		if !validChartVersion(ok) {
			t.Errorf("validChartVersion(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "1.0.0\n      urls:", "1.0.0: x", "../1.0.0", "1 0"} {
		if validChartVersion(bad) {
			t.Errorf("validChartVersion(%q) = true, want false", bad)
		}
	}
}

// TestBuildIndex_HostileRecordStaysParseable — the render-side guard, tested
// independently of upload validation because records predating that guard, and
// anything a proxied upstream sends, still reach it.
func TestBuildIndex_HostileRecordStaysParseable(t *testing.T) {
	recs := []chartRecord{
		{Name: "good", Version: "1.0.0", Filename: "good-1.0.0.tgz"},
		{Name: `evil: injected`, Version: `1.0.0
      urls:
        - https://evil.example/backdoor.tgz`, Filename: "evil-1.0.0.tgz"},
	}
	idx := buildIndex(recs, fixedNow)

	// The hostile value must not appear as live YAML structure.
	if bytes.Contains([]byte(idx), []byte("\n      urls:\n        - https://evil.example")) {
		t.Errorf("attacker-controlled URL injected as index structure:\n%s", idx)
	}
	// A plain chart must still render unquoted, so ordinary output is unchanged.
	if !bytes.Contains([]byte(idx), []byte("  good:\n")) {
		t.Errorf("ordinary chart no longer renders as a plain scalar:\n%s", idx)
	}
	if !bytes.Contains([]byte(idx), []byte("      version: 1.0.0\n")) {
		t.Errorf("ordinary version no longer renders as a plain scalar:\n%s", idx)
	}
}

func TestYamlScalar(t *testing.T) {
	for in, want := range map[string]string{
		"nginx":          "nginx",
		"1.0.0":          "1.0.0",
		"0.1.0-alpha.1":  "0.1.0-alpha.1",
		"evil: injected": `"evil: injected"`,
		"":               `""`,
		"a b":            `"a b"`,
	} {
		if got := yamlScalar(in); got != want {
			t.Errorf("yamlScalar(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpload_RejectsHostileChartMetadata drives the guard through Serve, so
// disabling the check at its call site is caught — testing the validator
// function alone is not enough.
func TestUpload_RejectsHostileChartMetadata(t *testing.T) {
	for _, tc := range []struct{ desc, name, version string }{
		{"name breaks the index mapping", "evil: injected", "1.0.0"},
		{"name climbs out of the repo", "../../../pwned", "1.0.0"},
		{"name nests a path", "a/b/c", "1.0.0"},
		{"version breaks the index", "ok", "1.0.0: x"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			c := newHostedCtx(t)
			rw := serve(c, "POST", "api/charts", bytesReader(makeChart(t, tc.name, tc.version)))
			if rw.Code < 400 {
				t.Errorf("upload accepted %s (HTTP %d); it must be refused at the door",
					tc.desc, rw.Code)
			}
			// And the index it would have produced is still parseable.
			idx := serve(c, "GET", "index.yaml", nil).Body.String()
			if strings.Contains(idx, "injected") || strings.Contains(idx, "pwned") {
				t.Errorf("hostile metadata reached the index:\n%s", idx)
			}
		})
	}
}
