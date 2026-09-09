package repo

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// The invariant belongs to the set of repositories, not to one HTTP handler.
// Every path that creates a repository — the admin API, the browser form, a
// Config-as-Code apply, a Nexus migration, the startup seed — goes through
// Add and Update, so each one inherits it here.
func TestManager_RefusesPublicGroupOverPrivateMember(t *testing.T) {
	m := NewManager()
	if err := m.Add(Repository{Name: "hosted", Format: "npm", Kind: Hosted, AnonymousRead: false}); err != nil {
		t.Fatal(err)
	}

	err := m.Add(Repository{Name: "pub", Format: "npm", Kind: Group,
		Members: []string{"hosted"}, AnonymousRead: true})
	if err == nil {
		t.Fatal("Add accepted a public group over a private member")
	}
	if !strings.Contains(err.Error(), "anonymous") {
		t.Errorf("error does not explain the exposure: %v", err)
	}
	if _, ok := m.Get("pub"); ok {
		t.Error("the refused group was stored anyway")
	}

	// The private version is fine, and so is a public group over public members.
	if err := m.Add(Repository{Name: "pub", Format: "npm", Kind: Group,
		Members: []string{"hosted"}}); err != nil {
		t.Fatalf("private group over a private member refused: %v", err)
	}
	// ...and it cannot be turned public afterwards.
	if err := m.Update(Repository{Name: "pub", Format: "npm", Kind: Group,
		Members: []string{"hosted"}, AnonymousRead: true}); err == nil {
		t.Error("Update turned a group public over a private member")
	}
	if g, _ := m.Get("pub"); g.AnonymousRead {
		t.Error("the refused update was applied anyway")
	}
}

// The same rule from the other side: a member cannot go private underneath a
// public group, which would leave the group serving it to anonymous clients.
func TestManager_RefusesPrivatingAMemberOfAPublicGroup(t *testing.T) {
	m := NewManager()
	mustAdd(t, m, Repository{Name: "hosted", Format: "npm", Kind: Hosted, AnonymousRead: true})
	mustAdd(t, m, Repository{Name: "pub", Format: "npm", Kind: Group,
		Members: []string{"hosted"}, AnonymousRead: true})

	if err := m.Update(Repository{Name: "hosted", Format: "npm", Kind: Hosted}); err == nil {
		t.Fatal("a member of a public group was allowed to go private")
	}
	if r, _ := m.Get("hosted"); !r.AnonymousRead {
		t.Error("the refused update was applied anyway")
	}
}

func TestManager_RefusesNestedAndSelfReferencingGroups(t *testing.T) {
	m := NewManager()
	mustAdd(t, m, Repository{Name: "hosted", Format: "npm", Kind: Hosted})
	mustAdd(t, m, Repository{Name: "inner", Format: "npm", Kind: Group, Members: []string{"hosted"}})

	if err := m.Add(Repository{Name: "outer", Format: "npm", Kind: Group, Members: []string{"inner"}}); err == nil {
		t.Error("a group containing a group was accepted")
	}
	if err := m.Add(Repository{Name: "self", Format: "npm", Kind: Group, Members: []string{"self"}}); err == nil {
		t.Error("a group listing itself was accepted")
	}
}

// A configuration stored before the check existed must not keep serving
// private content. Loading fails closed rather than refusing to start.
func TestManager_LoadClosesAPublicGroupOverAPrivateMember(t *testing.T) {
	store := newMemStore()
	m := NewManager()
	// Write the invalid pair straight to the store, as an older forge would have.
	store.PutJSON(repoNS, "hosted", Repository{Name: "hosted", Format: "npm", Kind: Hosted}) //nolint:errcheck
	store.PutJSON(repoNS, "pub", Repository{Name: "pub", Format: "npm", Kind: Group,         //nolint:errcheck
		Members: []string{"hosted"}, AnonymousRead: true})

	if err := m.WithStore(store); err != nil {
		t.Fatal(err)
	}
	g, ok := m.Get("pub")
	if !ok {
		t.Fatal("group was dropped entirely")
	}
	if g.AnonymousRead {
		t.Error("a stored public group over a private member stayed public after load")
	}
}

func mustAdd(t *testing.T, m *Manager, r Repository) {
	t.Helper()
	if err := m.Add(r); err != nil {
		t.Fatal(err)
	}
}

// memStore is a metaStore backed by a map, enough for the load path.
type memStore struct{ docs map[string][]byte }

func newMemStore() *memStore { return &memStore{docs: map[string][]byte{}} }

func (s *memStore) PutJSON(ns, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.docs[ns+"/"+key] = b
	return nil
}

func (s *memStore) GetJSON(ns, key string, v any) (bool, error) {
	b, ok := s.docs[ns+"/"+key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (s *memStore) List(ns string) ([]string, error) {
	var out []string
	for k := range s.docs {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, strings.TrimPrefix(k, ns+"/"))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *memStore) Delete(ns, key string) error {
	delete(s.docs, ns+"/"+key)
	return nil
}
