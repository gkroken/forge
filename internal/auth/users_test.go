package auth_test

import (
	"path/filepath"
	"testing"

	"forge/internal/auth"
	"forge/internal/meta"
)

func newUserStore(t *testing.T) auth.UserStore {
	t.Helper()
	m, err := meta.NewFS(filepath.Join(t.TempDir(), "meta"))
	if err != nil {
		t.Fatal(err)
	}
	return auth.NewUserStore(m)
}

func TestUserStore_CreateAndGet(t *testing.T) {
	us := newUserStore(t)
	u, err := us.Create("alice", "s3cr3t", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || u.Role != "Reader" {
		t.Fatalf("unexpected user: %+v", u)
	}

	got, ok, err := us.Get("alice")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Username != "alice" {
		t.Fatalf("got username %q, want alice", got.Username)
	}
}

func TestUserStore_DuplicateCreate(t *testing.T) {
	us := newUserStore(t)
	if _, err := us.Create("bob", "pw", "Reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := us.Create("bob", "pw2", "Publisher"); err == nil {
		t.Fatal("expected error for duplicate user")
	}
}

func TestUserStore_Authenticate(t *testing.T) {
	us := newUserStore(t)
	if _, err := us.Create("carol", "correct", "Administrator"); err != nil {
		t.Fatal(err)
	}

	// correct password
	got, err := us.Authenticate("carol", "correct")
	if err != nil || got == nil {
		t.Fatalf("expected auth success, got nil err=%v", err)
	}
	if got.LastLogin == nil {
		t.Fatal("expected LastLogin to be set")
	}

	// wrong password → nil, nil
	got, err = us.Authenticate("carol", "wrong")
	if err != nil || got != nil {
		t.Fatalf("expected nil for wrong password, got %v err=%v", got, err)
	}

	// unknown user → nil, nil
	got, err = us.Authenticate("ghost", "pw")
	if err != nil || got != nil {
		t.Fatalf("expected nil for unknown user, got %v err=%v", got, err)
	}
}

func TestUserStore_Disabled(t *testing.T) {
	us := newUserStore(t)
	if _, err := us.Create("dave", "pw", "Reader"); err != nil {
		t.Fatal(err)
	}
	if err := us.SetDisabled("dave", true); err != nil {
		t.Fatal(err)
	}
	// disabled user cannot authenticate
	got, err := us.Authenticate("dave", "pw")
	if err != nil || got != nil {
		t.Fatalf("disabled user should not authenticate, got %v err=%v", got, err)
	}
}

func TestUserStore_SetRole(t *testing.T) {
	us := newUserStore(t)
	if _, err := us.Create("eve", "pw", "Reader"); err != nil {
		t.Fatal(err)
	}
	if err := us.SetRole("eve", "Publisher"); err != nil {
		t.Fatal(err)
	}
	u, _, _ := us.Get("eve")
	if u.Role != "Publisher" {
		t.Fatalf("expected Publisher, got %q", u.Role)
	}
}

func TestUserStore_Delete(t *testing.T) {
	us := newUserStore(t)
	if _, err := us.Create("frank", "pw", "Reader"); err != nil {
		t.Fatal(err)
	}
	if err := us.Delete("frank"); err != nil {
		t.Fatal(err)
	}
	_, ok, _ := us.Get("frank")
	if ok {
		t.Fatal("expected user to be deleted")
	}
}

func TestUserStore_List(t *testing.T) {
	us := newUserStore(t)
	for _, name := range []string{"u1", "u2", "u3"} {
		if _, err := us.Create(name, "pw", "Reader"); err != nil {
			t.Fatal(err)
		}
	}
	users, err := us.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
}
