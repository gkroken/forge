package auth

import (
	"fmt"
	"strings"
)

// ParseGroupMappings parses a "group:role,group:role" string into GroupRules.
// Role is one of read|write|admin (reader|publisher|administrator also accepted).
// The group name is everything before the final colon, so it may itself contain
// colons. An empty string yields no rules. Shared by the OIDC and LDAP frontends.
func ParseGroupMappings(s string) ([]GroupRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var rules []GroupRule
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.LastIndex(pair, ":")
		if i < 0 {
			return nil, fmt.Errorf("group mapping %q: expected group:role", pair)
		}
		group := strings.TrimSpace(pair[:i])
		roleName := strings.TrimSpace(pair[i+1:])
		if group == "" {
			return nil, fmt.Errorf("group mapping %q: empty group name", pair)
		}
		role := BaseRoleFor(roleName)
		if role == RoleNone {
			return nil, fmt.Errorf("group mapping %q: unknown role %q (want read|write|admin)", pair, roleName)
		}
		rules = append(rules, GroupRule{Group: group, Role: role})
	}
	return rules, nil
}

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
