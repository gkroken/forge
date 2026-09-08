package pypi_test

import (
	"testing"

	"forge/internal/format"
	"forge/internal/format/pypi"
)

// TestImplementsHandler — the handler must satisfy every seam, or fail to build.
var _ format.Handler = pypi.New()

// TestNormalize covers PEP 503: forge and pip must agree on what a project is
// called, or a package published under one spelling is invisible when requested
// by another.
func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Foo.Bar", "foo-bar"},
		{"foo_bar", "foo-bar"},
		{"foo--bar", "foo-bar"},
		{"Foo._.Bar", "foo-bar"},
		{"already-normal", "already-normal"},
		{"Django", "django"},
		{"zope.interface", "zope-interface"},
	} {
		if got := pypi.Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOSVCoordinates — advisories are keyed by the normalized name, so a lookup
// for "Foo.Bar" has to ask OSV about "foo-bar" or every advisory is missed.
func TestOSVCoordinates(t *testing.T) {
	h := pypi.New()
	if eco := h.OSVEcosystem(); eco != "PyPI" {
		t.Errorf("OSVEcosystem = %q, want PyPI", eco)
	}
	eco, name, ok := h.OSVCoordinates("Foo.Bar")
	if !ok || eco != "PyPI" || name != "foo-bar" {
		t.Errorf("OSVCoordinates(Foo.Bar) = (%q,%q,%v)", eco, name, ok)
	}
}

// TestClaimPath — dependency confusion bites at resolution, not download: pip
// asks the simple index for a name long before it fetches a file, so a claim
// must cover that path too.
func TestClaimPath(t *testing.T) {
	h := pypi.New()
	for _, tc := range []struct {
		sub  string
		want string
		ok   bool
	}{
		{"simple/My.Package/", "my-package", true},
		{"simple/internal-tool", "internal-tool", true},
		{"packages/internal-tool/internal_tool-1.0.0-py3-none-any.whl", "internal-tool", true},
		{"simple/", "", false},
		{"nonsense", "", false},
	} {
		got, ok := h.ClaimPath(tc.sub)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ClaimPath(%q) = (%q,%v), want (%q,%v)", tc.sub, got, ok, tc.want, tc.ok)
		}
	}
}

// TestVulnGateTarget — the gate acts on a download, so it has to recover the
// release from a filename. Wheels replace "-" with "_" in the distribution
// part, which is why the prefix is normalized rather than compared literally.
func TestVulnGateTarget(t *testing.T) {
	h := pypi.New()
	for _, tc := range []struct {
		sub       string
		comp, ver string
		ok        bool
	}{
		{"packages/my-package/my_package-1.2.0-py3-none-any.whl", "my-package", "1.2.0", true},
		{"packages/my-package/my_package-1.2.0.tar.gz", "my-package", "1.2.0", true},
		{"packages/other/my_package-1.2.0.tar.gz", "", "", false}, // filename disagrees with the path
		{"simple/my-package/", "", "", false},                     // not a download
	} {
		comp, ver, ok := h.VulnGateTarget(tc.sub)
		if comp != tc.comp || ver != tc.ver || ok != tc.ok {
			t.Errorf("VulnGateTarget(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.sub, comp, ver, ok, tc.comp, tc.ver, tc.ok)
		}
	}
}
