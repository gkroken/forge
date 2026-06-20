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
