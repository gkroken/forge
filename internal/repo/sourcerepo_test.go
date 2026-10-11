package repo_test

import (
	"strings"
	"testing"

	"forge/internal/repo"
)

// A binary repository forge accepts but can never serve, or whose check a
// hosted package can pass, is reported (forge-issues/005).
func TestSourceRepoWarnings(t *testing.T) {
	base := []repo.Repository{
		{Name: "cran-hosted", Format: "cran", Kind: repo.Hosted},
		{Name: "cran-proxy", Format: "cran", Kind: repo.Proxy, Upstream: "http://x"},
	}
	bin := func(name, src string) repo.Repository {
		return repo.Repository{Name: name, Format: "cran", Kind: repo.Hosted, NoOverwrite: true, SourceRepo: src}
	}
	group := func(members ...string) repo.Repository {
		return repo.Repository{Name: "cran-public", Format: "cran", Kind: repo.Group, Members: members}
	}
	for _, tc := range []struct {
		name  string
		repos []repo.Repository
		want  []string // one substring per expected warning, in order
	}{
		{name: "proxy member, in the group", repos: []repo.Repository{bin("cran-linux", "cran-proxy"),
			group("cran-hosted", "cran-proxy", "cran-linux")}},
		{name: "hosted member, in the group", repos: []repo.Repository{bin("cran-int", "cran-hosted"),
			group("cran-hosted", "cran-proxy", "cran-int")}},
		{name: "in no group", repos: []repo.Repository{bin("cran-linux", "cran-proxy"), group("cran-hosted", "cran-proxy")},
			want: []string{`"cran-linux" has sourceRepo "cran-proxy" but no group lists it`}},
		{name: "sourceRepo not a member", repos: []repo.Repository{bin("cran-linux", "cran-proxy"), group("cran-hosted", "cran-linux")},
			want: []string{`group "cran-public" never serves "cran-linux"'s binaries`}},
		{name: "the group, with a hosted member", repos: []repo.Repository{bin("cran-linux", "cran-public"),
			group("cran-hosted", "cran-proxy", "cran-linux")},
			want: []string{`"cran-linux" checks binaries against group "cran-public", which also answers from hosted cran-hosted:`}},
		{name: "the group, only binary repositories hosted", repos: []repo.Repository{bin("cran-linux", "cran-public"),
			bin("cran-int", "cran-hosted"), group("cran-proxy", "cran-linux", "cran-int")},
			// cran-int's own sourceRepo is not in the group; cran-linux is fine.
			want: []string{`group "cran-public" never serves "cran-int"'s binaries`}},
	} {
		m := repo.NewManager()
		for _, r := range append(append([]repo.Repository{}, base...), tc.repos...) {
			if err := m.Add(r); err != nil {
				t.Fatalf("%s: add %s: %v", tc.name, r.Name, err)
			}
		}
		got := m.SourceRepoWarnings()
		if len(got) != len(tc.want) {
			t.Errorf("%s: %d warnings %+v, want %d", tc.name, len(got), got, len(tc.want))
			continue
		}
		for i, w := range tc.want {
			if !strings.Contains(got[i].Msg, w) {
				t.Errorf("%s: warning %q, want it to contain %q", tc.name, got[i].Msg, w)
			}
		}
	}
}
