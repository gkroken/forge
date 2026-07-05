package format

import (
	"testing"

	"forge/internal/repo"
)

// MemberCtx must honor MemberFilter: the dependency-confusion guard uses it to
// exclude proxy members from group fan-outs for protected names.
func TestMemberCtx_MemberFilter(t *testing.T) {
	m := repo.NewManager()
	m.Add(repo.Repository{Name: "hosted", Format: "npm", Kind: repo.Hosted, Enabled: true})
	m.Add(repo.Repository{Name: "proxy", Format: "npm", Kind: repo.Proxy, Upstream: "http://up", Enabled: true})
	m.Add(repo.Repository{Name: "nested", Format: "npm", Kind: repo.Group, Members: []string{"hosted"}, Enabled: true})

	c := &Context{
		Repo:  repo.Repository{Name: "g", Format: "npm", Kind: repo.Group, Members: []string{"hosted", "proxy"}},
		Repos: m,
		Sub:   "@acme/foo",
	}

	// No filter: both members resolve.
	for _, name := range []string{"hosted", "proxy"} {
		if _, ok := c.MemberCtx(name); !ok {
			t.Fatalf("unfiltered MemberCtx(%q) = false, want true", name)
		}
	}
	// Groups never nest, filter or not.
	if _, ok := c.MemberCtx("nested"); ok {
		t.Fatal("MemberCtx allowed a nested group")
	}

	// Filter that rejects proxy members (what the dep-confusion guard installs).
	c.MemberFilter = func(r repo.Repository) bool { return r.Kind != repo.Proxy }
	if _, ok := c.MemberCtx("hosted"); !ok {
		t.Fatal("filtered MemberCtx rejected a hosted member")
	}
	if _, ok := c.MemberCtx("proxy"); ok {
		t.Fatal("filtered MemberCtx allowed a proxy member")
	}
}
