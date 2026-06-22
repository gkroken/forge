package auth

import "strings"

// GroupRule maps a single identity-provider group name onto a base Role.
// Group matching is case-insensitive (see GroupRoleMapper.Resolve).
type GroupRule struct {
	Group string `json:"group"`
	Role  Role   `json:"role"`
}

// GroupRoleMapper translates a set of IdP group memberships into a forge Role.
//
// It is transport-neutral: it knows nothing about OIDC, LDAP, or how the group
// list was obtained. Any authentication frontend that can produce a list of
// group names (OIDC groups claim today, an LDAP memberOf lookup later) feeds the
// same mapper, so role mapping is defined in exactly one place.
type GroupRoleMapper struct {
	rules []GroupRule
}

// NewGroupRoleMapper returns a mapper over the given rules. The slice is copied,
// so the caller may reuse or mutate the original.
func NewGroupRoleMapper(rules []GroupRule) *GroupRoleMapper {
	cp := make([]GroupRule, len(rules))
	copy(cp, rules)
	return &GroupRoleMapper{rules: cp}
}

// Rules returns a copy of the configured rules (for read-only display).
func (m *GroupRoleMapper) Rules() []GroupRule {
	if m == nil {
		return nil
	}
	cp := make([]GroupRule, len(m.rules))
	copy(cp, m.rules)
	return cp
}

// Resolve returns the highest Role granted by any rule whose group appears in
// groups. Matching is case-insensitive. matched is false when no rule applies,
// in which case role is RoleNone and the caller should fall back to its default
// grants. A nil mapper resolves to (RoleNone, false).
func (m *GroupRoleMapper) Resolve(groups []string) (role Role, matched bool) {
	if m == nil || len(m.rules) == 0 || len(groups) == 0 {
		return RoleNone, false
	}
	have := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		have[strings.ToLower(g)] = struct{}{}
	}
	best := RoleNone
	for _, rule := range m.rules {
		if _, ok := have[strings.ToLower(rule.Group)]; ok {
			if rule.Role > best {
				best = rule.Role
			}
		}
	}
	return best, best != RoleNone
}
