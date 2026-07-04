// Package selector implements the shared content-selector grammar: glob
// patterns matched against a repository-relative content path. It is used by
// grant scoping (auth) and is designed for reuse by dependency-confusion
// protection, which needs the same "does this name belong to this namespace"
// question.
//
// Grammar:
//   - a pattern is a "/"-separated list of segments matched against the
//     path's segments
//   - within a segment, "*" matches any run of characters except "/"
//   - a segment consisting solely of "**" matches zero or more whole segments
//   - every other character matches itself, case-sensitively
//
// Examples:
//
//	com/acme/**    everything under com/acme/ (Maven groupId com.acme)
//	@acme/**       npm packages in the @acme scope
//	acme-*         top-level entries whose name starts with acme-
package selector

import (
	"fmt"
	"strings"
)

// Validate reports whether pattern is well-formed. It rejects empty patterns,
// leading/trailing or doubled slashes, and "**" mixed with other characters
// inside one segment (e.g. "a**b"), which has no defined meaning.
func Validate(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("selector must not be empty")
	}
	if strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") {
		return fmt.Errorf("selector %q: must not start or end with /", pattern)
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" {
			return fmt.Errorf("selector %q: empty path segment", pattern)
		}
		if seg != "**" && strings.Contains(seg, "**") {
			return fmt.Errorf("selector %q: ** must be a whole segment", pattern)
		}
	}
	return nil
}

// Match reports whether path matches pattern. A malformed pattern matches
// nothing (fail closed); callers are expected to Validate at write time.
// Leading slashes on path are ignored so both "com/acme/a.jar" and
// "/com/acme/a.jar" behave identically.
func Match(pattern, path string) bool {
	if Validate(pattern) != nil {
		return false
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return false
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// MatchAny reports whether path matches at least one of patterns.
func MatchAny(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

func matchSegs(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		// Try consuming zero segments, then one more at a time.
		if matchSegs(pat[1:], path) {
			return true
		}
		if len(path) == 0 {
			return false
		}
		return matchSegs(pat, path[1:])
	}
	if len(path) == 0 {
		return false
	}
	if !matchSeg(pat[0], path[0]) {
		return false
	}
	return matchSegs(pat[1:], path[1:])
}

// matchSeg matches one pattern segment against one path segment, where "*"
// matches any run of characters. Iterative with single-star backtracking.
func matchSeg(pat, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pat) && pat[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}
