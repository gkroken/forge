package maven

import (
	"encoding/xml"
	"strings"
	"testing"
)

// Maven coordinates come from request paths, so a version directory can carry
// XML metacharacters. Nothing here was escaped: a version containing "<"
// produced malformed maven-metadata.xml, and every Maven client then failed to
// parse it — breaking resolution of that artifact for everyone, not just the
// publisher.

func TestBuildMetadataXML_EscapesCoordinates(t *testing.T) {
	out := buildMetadataXML("com.acme", "widget", []string{
		"1.0.0",
		"1.0</version><injected>x</injected><version>",
		"1.0&amp;",
		`1.0"quote'`,
	})
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Fatalf("generated metadata is not well-formed XML: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "<injected>") {
		t.Errorf("injected element survived escaping:\n%s", out)
	}
	// The ordinary version must still read back exactly.
	var md struct {
		Versioning struct {
			Versions struct {
				Version []string `xml:"version"`
			} `xml:"versions"`
		} `xml:"versioning"`
	}
	if err := xml.Unmarshal(out, &md); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var found bool
	for _, v := range md.Versioning.Versions.Version {
		if v == "1.0.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("ordinary version lost: %v", md.Versioning.Versions.Version)
	}
}

func TestXMLText(t *testing.T) {
	for in, want := range map[string]string{
		"1.0.0": "1.0.0",
		"a<b":   "a&lt;b",
		"a&b":   "a&amp;b",
		`a"b`:   "a&#34;b",
	} {
		if got := xmlText(in); got != want {
			t.Errorf("xmlText(%q) = %q, want %q", in, got, want)
		}
	}
}
