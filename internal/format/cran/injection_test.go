package cran

import "testing"

// A DESCRIPTION file is one DCF record, and a blank line ends it. scanDescription
// skipped blank lines instead, so a second record in the same file overwrote the
// first: a package uploaded as victim_1.0.0.tar.gz was indexed under whatever
// name the trailing record declared, and the real one disappeared from PACKAGES.
func TestScanDescription_StopsAtTheRecordBoundary(t *testing.T) {
	rec := scanDescription([]byte(
		"Package: victim\nVersion: 1.0.0\nLicense: MIT\n\nPackage: injected\nVersion: 9.9.9\n"))
	if rec.Package != "victim" || rec.Version != "1.0.0" {
		t.Errorf("got %s_%s, want victim_1.0.0 — fields after the blank line are a "+
			"separate DCF record and must be ignored", rec.Package, rec.Version)
	}
}

func TestScanDescription_KeepsContinuationLines(t *testing.T) {
	rec := scanDescription([]byte(
		"Package: cont\nVersion: 1.0.0\nTitle: a title\n  continued here\nLicense: MIT\n"))
	if rec.Package != "cont" || rec.License != "MIT" {
		t.Errorf("continuation handling broke the record: %+v", rec)
	}
	if rec.Title != "a title continued here" {
		t.Errorf("Title = %q, want the folded continuation", rec.Title)
	}
}

// CRAN names a source package {Package}_{Version}.tar.gz. The blob is stored
// under the URL filename while the index entry comes from DESCRIPTION, so a
// mismatch advertises a package whose file is not there.
func TestNameVersionFromFilename(t *testing.T) {
	for _, tc := range []struct {
		sub, pkg, ver string
		ok            bool
	}{
		{"src/contrib/jsonlite_1.8.9.tar.gz", "jsonlite", "1.8.9", true},
		{"src/contrib/my.pkg_0.1.0-2.tar.gz", "my.pkg", "0.1.0-2", true},
		{"src/contrib/nounderscore.tar.gz", "", "", false},
	} {
		pkg, ver, ok := nameVersionFromFilename(tc.sub)
		if ok != tc.ok || pkg != tc.pkg || ver != tc.ver {
			t.Errorf("nameVersionFromFilename(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.sub, pkg, ver, ok, tc.pkg, tc.ver, tc.ok)
		}
	}
}
