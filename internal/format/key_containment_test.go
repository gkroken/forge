package format_test

import (
	"path"
	"strings"
	"testing"

	"forge/internal/format"
	"forge/internal/repo"
)

// Key must contain every sub-path inside the repository, whatever the caller
// passes, because not every sub-path comes from a URL that net/http cleaned.
func TestKeyStaysInsideTheRepository(t *testing.T) {
	c := &format.Context{Repo: repo.Repository{Name: "acme-npm"}}
	for _, sub := range []string{
		"../../npmjs/is-odd/-/is-odd-3.0.1.tgz",
		"../../../../pwned.tgz",
		"pkg/-/../../../escape.tgz",
		"..",
	} {
		got := c.Key(sub)
		// Checking the literal prefix is not enough: "acme-npm/../../x" starts
		// with "acme-npm/" and still escapes once a filesystem resolves it.
		resolved := path.Clean("/" + got)
		// Collapsing to the repository directory itself is containment, not
		// escape; anything above it is escape.
		if resolved != "/acme-npm" && !strings.HasPrefix(resolved, "/acme-npm/") {
			t.Errorf("Key(%q) = %q, resolves to %q — escapes the repository", sub, got, resolved)
		}
	}
	if got := c.Key("pkg/-/pkg-1.0.0.tgz"); got != "acme-npm/pkg/-/pkg-1.0.0.tgz" {
		t.Errorf("ordinary key changed shape: %q", got)
	}
	if got := c.Key(""); got != "acme-npm/" {
		t.Errorf("empty sub = %q, want the repo prefix", got)
	}
}
