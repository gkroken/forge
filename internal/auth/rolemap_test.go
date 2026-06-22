package auth

import "testing"

func TestGroupRoleMapper_Resolve(t *testing.T) {
	rules := []GroupRule{
		{Group: "forge-admins", Role: RoleAdmin},
		{Group: "developers", Role: RoleWrite},
		{Group: "staff", Role: RoleRead},
	}
	m := NewGroupRoleMapper(rules)

	tests := []struct {
		name    string
		groups  []string
		want    Role
		matched bool
	}{
		{"single match", []string{"developers"}, RoleWrite, true},
		{"highest wins", []string{"staff", "forge-admins", "developers"}, RoleAdmin, true},
		{"case-insensitive group", []string{"Forge-Admins"}, RoleAdmin, true},
		{"no match", []string{"contractors"}, RoleNone, false},
		{"empty groups", nil, RoleNone, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := m.Resolve(tc.groups)
			if got != tc.want || matched != tc.matched {
				t.Fatalf("Resolve(%v) = (%v, %v), want (%v, %v)",
					tc.groups, got, matched, tc.want, tc.matched)
			}
		})
	}
}

func TestGroupRoleMapper_NilAndEmpty(t *testing.T) {
	var nilMapper *GroupRoleMapper
	if role, matched := nilMapper.Resolve([]string{"x"}); role != RoleNone || matched {
		t.Fatalf("nil mapper: got (%v, %v), want (none, false)", role, matched)
	}
	empty := NewGroupRoleMapper(nil)
	if role, matched := empty.Resolve([]string{"x"}); role != RoleNone || matched {
		t.Fatalf("empty mapper: got (%v, %v), want (none, false)", role, matched)
	}
	if empty.Rules() == nil {
		t.Fatal("Rules() should return non-nil empty slice")
	}
}

func TestGroupRoleMapper_RulesIsCopy(t *testing.T) {
	rules := []GroupRule{{Group: "a", Role: RoleRead}}
	m := NewGroupRoleMapper(rules)
	rules[0].Role = RoleAdmin // mutate caller's slice
	if got, _ := m.Resolve([]string{"a"}); got != RoleRead {
		t.Fatalf("mapper should hold a copy; got %v", got)
	}
	out := m.Rules()
	out[0].Role = RoleAdmin // mutate returned slice
	if got, _ := m.Resolve([]string{"a"}); got != RoleRead {
		t.Fatalf("Rules() should return a copy; got %v", got)
	}
}
