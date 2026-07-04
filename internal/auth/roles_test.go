package auth_test

import (
	"path/filepath"
	"testing"

	"forge/internal/auth"
	"forge/internal/meta"
)

func newRoleStore(t *testing.T) auth.RoleStore {
	t.Helper()
	m, err := meta.NewFS(filepath.Join(t.TempDir(), "meta"))
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewRoleStore(m)
}

func TestRoleStore_CreateAndGet(t *testing.T) {
	rs := newRoleStore(t)
	role := auth.CustomRole{Name: "ci-deployer", Description: "CI publish only", BaseRole: "write"}
	if err := rs.Create(role); err != nil {
		t.Fatal(err)
	}
	got, ok, err := rs.Get("ci-deployer")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.BaseRole != "write" {
		t.Fatalf("expected write, got %q", got.BaseRole)
	}
}

func TestRoleStore_DuplicateCreate(t *testing.T) {
	rs := newRoleStore(t)
	role := auth.CustomRole{Name: "dev", BaseRole: "read"}
	if err := rs.Create(role); err != nil {
		t.Fatal(err)
	}
	if err := rs.Create(role); err == nil {
		t.Fatal("expected error for duplicate role")
	}
}

func TestRoleStore_PredefinedProtected(t *testing.T) {
	rs := newRoleStore(t)

	// Cannot create a role with a predefined name.
	if err := rs.Create(auth.CustomRole{Name: "Reader", BaseRole: "read"}); err == nil {
		t.Fatal("expected error when creating predefined role")
	}

	// Cannot delete a predefined role.
	if err := rs.Delete("Administrator"); err == nil {
		t.Fatal("expected error when deleting predefined role")
	}
}

func TestRoleStore_Delete(t *testing.T) {
	rs := newRoleStore(t)
	if err := rs.Create(auth.CustomRole{Name: "tmp", BaseRole: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Delete("tmp"); err != nil {
		t.Fatal(err)
	}
	_, ok, _ := rs.Get("tmp")
	if ok {
		t.Fatal("expected role to be deleted")
	}
}

func TestRoleStore_List(t *testing.T) {
	rs := newRoleStore(t)
	for _, name := range []string{"r1", "r2"} {
		if err := rs.Create(auth.CustomRole{Name: name, BaseRole: "read"}); err != nil {
			t.Fatal(err)
		}
	}
	roles, err := rs.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 {
		t.Fatalf("expected 2 custom roles, got %d", len(roles))
	}
}

func TestBaseRoleFor(t *testing.T) {
	cases := []struct {
		name string
		want auth.Role
	}{
		{"Reader", auth.RoleRead},
		{"read", auth.RoleRead},
		{"Publisher", auth.RoleWrite},
		{"write", auth.RoleWrite},
		{"Administrator", auth.RoleAdmin},
		{"admin", auth.RoleAdmin},
		{"unknown", auth.RoleNone},
	}
	for _, c := range cases {
		if got := auth.BaseRoleFor(c.name); got != c.want {
			t.Errorf("BaseRoleFor(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRoleStore_GrantsValidatedOnCreate(t *testing.T) {
	rs := newRoleStore(t)
	bad := auth.CustomRole{Name: "bad", BaseRole: "read", Grants: []auth.Grant{
		{Repo: "libs", Actions: []auth.Action{auth.ActionAdmin}, Selectors: []string{"com/**"}},
	}}
	if err := rs.Create(bad); err == nil {
		t.Fatal("admin+selector grant must be rejected on role create")
	}
	good := auth.CustomRole{Name: "good", BaseRole: "read", Grants: []auth.Grant{
		{Repo: "libs", Actions: []auth.Action{auth.ActionRead}, Selectors: []string{"com/acme/**"}},
	}}
	if err := rs.Create(good); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := rs.Get("good")
	if !ok || len(got.Grants) != 1 {
		t.Fatalf("grants not persisted: %+v ok=%v", got, ok)
	}
}

func TestGrantsForRoleName(t *testing.T) {
	rs := newRoleStore(t)
	granted := []auth.Grant{
		{Repo: "libs", Actions: []auth.Action{auth.ActionRead, auth.ActionWrite}},
		{Repo: "docs", Actions: []auth.Action{auth.ActionRead}},
	}
	if err := rs.Create(auth.CustomRole{Name: "nexus-devs", BaseRole: "write", Grants: granted}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Create(auth.CustomRole{Name: "tier-only", BaseRole: "write"}); err != nil {
		t.Fatal(err)
	}

	t.Run("custom role with grants uses them verbatim", func(t *testing.T) {
		got := auth.GrantsForRoleName(rs, "nexus-devs")
		if len(got) != 2 || got[0].Repo != "libs" || len(got[0].Actions) != 2 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("custom role without grants expands its tier", func(t *testing.T) {
		got := auth.GrantsForRoleName(rs, "tier-only")
		if len(got) != 1 || got[0].Repo != "*" {
			t.Fatalf("got %+v", got)
		}
		tok := auth.Token{Grants: got}
		if !tok.Allows("anything", "", auth.ActionWrite) || tok.GlobalAdmin() {
			t.Fatalf("tier-only should be instance-wide write, not admin: %+v", got)
		}
	})
	t.Run("predefined names work without a store hit", func(t *testing.T) {
		got := auth.GrantsForRoleName(rs, "Administrator")
		if len(got) != 1 || !(&auth.Token{Grants: got}).GlobalAdmin() {
			t.Fatalf("got %+v", got)
		}
		if auth.GrantsForRoleName(nil, "read") == nil {
			t.Fatal("nil store must still resolve base names")
		}
	})
	t.Run("unknown role resolves to nothing", func(t *testing.T) {
		if got := auth.GrantsForRoleName(rs, "ghost"); got != nil {
			t.Fatalf("got %+v", got)
		}
	})
}
