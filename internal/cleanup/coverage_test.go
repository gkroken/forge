package cleanup_test

import (
	"strings"
	"testing"

	"forge/internal/cleanup"
	"forge/internal/repo"
)

// Format coverage roll call.
//
// Some seams no compiler can check: a switch in another package, a string in a
// JavaScript file. This is the backstop for those — a table of what every
// format is expected to support, checked against reality by probing each
// surface. The roll call at the top is the important half: adding a format
// fails this test until someone states, deliberately, what it supports.
//
// Everything the compiler CAN check belongs on format.Handler instead. Do not
// grow this table to cover things that could be interface methods.

type coverage struct {
	retention    bool // internal/cleanup, via format.Handler — compiler-checked
	trash        bool // internal/cleanup/trash.go switch (loud default)
	browseAsTree bool // internal/server/static/browse.js dispatch
}

// expected is the declared support matrix. A new format must be added here.
var expected = map[string]coverage{
	"maven": {retention: true, trash: true, browseAsTree: true},
	"npm":   {retention: true, trash: true},
	"helm":  {retention: true, trash: true},
	"cran":  {retention: true, trash: true},
	"oci":   {retention: true},
	"pypi":  {retention: true, trash: true},
}

// TestFormatCoverage_RollCall — every registered format must be declared above.
// This is what makes the rest of the file meaningful: without it, a new format
// simply is not tested rather than failing.
func TestFormatCoverage_RollCall(t *testing.T) {
	registered := formats().(interface{ Formats() []string }).Formats()
	for _, f := range registered {
		if _, ok := expected[f]; !ok {
			t.Errorf("format %q is registered but not declared in the coverage table — "+
				"state what it supports (retention, trash, browse-as-tree) and wire up whatever it needs", f)
		}
	}
	for f := range expected {
		found := false
		for _, r := range registered {
			if r == f {
				found = true
			}
		}
		if !found {
			t.Errorf("coverage table declares %q, which is not registered — stale entry", f)
		}
	}
}

// TestFormatCoverage_Retention probes the retention path per format. Retention
// is dispatched through format.Handler, so this catches a format that embeds
// Unsupported and never overrides ListVersions.
func TestFormatCoverage_Retention(t *testing.T) {
	for f, want := range expected {
		b, m := stores(t)
		_, err := cleanup.DryRun(rp("probe", f), formats(), &noopPolicy, b, m)
		supported := err == nil || !strings.Contains(err.Error(), "no retention for format")
		if supported != want.retention {
			t.Errorf("format %q retention support = %v, table says %v (err: %v)",
				f, supported, want.retention, err)
		}
	}
}

// TestFormatCoverage_Trash probes the trash switch, which is deliberately not
// interface-driven — restore semantics are more than a delete — so nothing but
// a test can tell us a format was left out of it.
func TestFormatCoverage_Trash(t *testing.T) {
	for f, want := range expected {
		b, m := stores(t)
		_, err := cleanup.TrashVersion("probe", f, "some-component", "1.0.0", "tester", b, m)
		unsupported := err != nil && strings.Contains(err.Error(), "unsupported format")
		if unsupported == want.trash {
			t.Errorf("format %q trash support = %v, table says %v (err: %v)",
				f, !unsupported, want.trash, err)
		}
	}
}

// TestFormatCoverage_BrowseTree checks the browse layout each format declares.
//
// This used to grep browse.js for the format name, because the tree-vs-flat
// decision was a string comparison in JavaScript. It is a fact about a format's
// storage layout, so it moved onto the handler and the JS now reads a flag the
// server passes down. The check stays to pin the declaration; it no longer has
// to reach into another language to do it.
func TestFormatCoverage_BrowseTree(t *testing.T) {
	res := formats()
	for f, want := range expected {
		h, ok := res.For(f)
		if !ok {
			t.Errorf("format %q is not registered", f)
			continue
		}
		if got := h.BrowseAsTree(); got != want.browseAsTree {
			t.Errorf("format %q BrowseAsTree = %v, table says %v", f, got, want.browseAsTree)
		}
	}
}

// noopPolicy keeps a version of everything, so the probe exercises dispatch
// without depending on what is (not) stored.
var noopPolicy = repo.CleanupPolicy{KeepVersions: 99}
