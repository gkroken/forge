package auth

import (
	"fmt"
	"strings"

	"forge/internal/meta"
)

const nsRoles = "auth:roles"

// PredefinedRoles are always available and cannot be deleted.
var PredefinedRoles = []CustomRole{
	{Name: "Reader", Description: "Pull and resolve artifacts from any repository. No publish rights.", BaseRole: "read"},
	{Name: "Publisher", Description: "Deploy to hosted repositories and trigger proxy prefetch. No config access.", BaseRole: "write"},
	{Name: "Administrator", Description: "Full control — repositories, tokens, cleanup, and system settings.", BaseRole: "admin"},
}

// CustomRole is a named permission tier.
type CustomRole struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	BaseRole    string `json:"baseRole"` // "read" | "write" | "admin"
}

// BaseRoleFor converts a role name (predefined or base) to a Role int.
func BaseRoleFor(name string) Role {
	switch strings.ToLower(name) {
	case "reader", "read":
		return RoleRead
	case "publisher", "write":
		return RoleWrite
	case "administrator", "admin":
		return RoleAdmin
	}
	return RoleNone
}

// RoleStore manages custom role definitions (predefined roles are hardcoded).
type RoleStore interface {
	Create(role CustomRole) error
	List() ([]CustomRole, error)
	Get(name string) (CustomRole, bool, error)
	Delete(name string) error
}

type roleMetaStore struct{ m meta.Store }

// NewRoleStore returns a RoleStore backed by m.
func NewRoleStore(m meta.Store) RoleStore { return &roleMetaStore{m: m} }

func isPredefined(name string) bool {
	for _, p := range PredefinedRoles {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

func (s *roleMetaStore) Create(role CustomRole) error {
	if role.Name == "" {
		return fmt.Errorf("role name required")
	}
	if isPredefined(role.Name) {
		return fmt.Errorf("role %q is predefined and cannot be recreated", role.Name)
	}
	var existing CustomRole
	if ok, _ := s.m.GetJSON(nsRoles, role.Name, &existing); ok {
		return fmt.Errorf("role %q already exists", role.Name)
	}
	return s.m.PutJSON(nsRoles, role.Name, role)
}

func (s *roleMetaStore) List() ([]CustomRole, error) {
	keys, err := s.m.List(nsRoles)
	if err != nil {
		return nil, err
	}
	out := make([]CustomRole, 0, len(keys))
	for _, k := range keys {
		var r CustomRole
		if ok, _ := s.m.GetJSON(nsRoles, k, &r); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *roleMetaStore) Get(name string) (CustomRole, bool, error) {
	var r CustomRole
	ok, err := s.m.GetJSON(nsRoles, name, &r)
	return r, ok, err
}

func (s *roleMetaStore) Delete(name string) error {
	if isPredefined(name) {
		return fmt.Errorf("predefined role %q cannot be deleted", name)
	}
	return s.m.Delete(nsRoles, name)
}
