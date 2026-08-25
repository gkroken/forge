package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/cleanup"
	"forge/internal/ldap"
	"forge/internal/repo"
	"sigs.k8s.io/yaml"
)

// Strict key checking for config files.
//
// encoding/json silently discards keys that match no field, so a config file
// with `anonymusRead: true` or `cleanupPolicyNam: keep-10` used to load clean
// and quietly do the wrong thing. That was survivable when config was an
// additive seed loader; now that the file is the source of truth it is not — a
// typo'd `prune` silently stops deletions, and a typo'd `claims` silently
// disables dependency-confusion protection for a repository.
//
// json.Decoder.DisallowUnknownFields cannot fix this on its own: a custom
// UnmarshalJSON receives raw bytes and decodes them itself, opting its whole
// type out of the outer decoder's strictness — and repo.Repository,
// repo.CleanupPolicy, cleanup.NamedPolicy, auth.Grant and ldap.Config all have
// one. Making those strict would also make them strict when reading records
// back out of the meta store, turning a forward-compatible read into a hard
// error. So the check lives here instead: it walks the decoded document against
// the field names reflected off File, and applies only to file loading.

// wireExtras lists keys a type's custom UnmarshalJSON accepts that have no
// matching tagged field — durations carried as strings, and Grant's "role"
// shorthand. They are accepted; their contents are not descended into, because
// their shape is defined by hand-written code rather than by struct tags.
var wireExtras = map[reflect.Type][]string{
	reflect.TypeOf(repo.Repository{}):     {"proxyTTL", "contentMaxAge", "metadataMaxAge"},
	reflect.TypeOf(repo.CleanupPolicy{}):  {"interval"},
	reflect.TypeOf(cleanup.NamedPolicy{}): {"interval"},
	reflect.TypeOf(auth.Grant{}):          {"role"},
	reflect.TypeOf(ldap.Config{}):         {"tokenTTL", "timeout"},
}

// opaque types are structs that carry a scalar wire form; do not descend.
var opaque = map[reflect.Type]bool{
	reflect.TypeOf(time.Time{}): true,
}

// CheckKeys reports every key in the document that matches no known field,
// each with its path and, where one is obvious, the field it was probably
// meant to be.
func CheckKeys(data []byte, asYAML bool) error {
	var doc any
	if asYAML {
		// UnmarshalStrict additionally rejects duplicate YAML keys, which would
		// otherwise silently keep the last one.
		if err := yaml.UnmarshalStrict(data, &doc); err != nil {
			return err
		}
	} else {
		if err := yaml.Unmarshal(data, &doc); err != nil { // JSON ⊂ YAML
			return err
		}
	}
	var errs []string
	walk(doc, reflect.TypeOf(File{}), "", &errs)
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("unknown field(s):\n  %s", strings.Join(errs, "\n  "))
}

func walk(doc any, t reflect.Type, path string, errs *[]string) {
	if doc == nil {
		return
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if opaque[t] {
			return
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return // type mismatch; the real decoder reports it with better context
		}
		fields := jsonFields(t)
		extras := make(map[string]bool, len(wireExtras[t]))
		for _, k := range wireExtras[t] {
			extras[k] = true
		}
		for k, v := range m {
			if ft, ok := fields[k]; ok {
				walk(v, ft, join(path, k), errs)
				continue
			}
			if extras[k] {
				continue // accepted by a custom unmarshaler; shape unknown
			}
			*errs = append(*errs, describe(join(path, k), k, fields, extras))
		}
	case reflect.Slice, reflect.Array:
		items, ok := doc.([]any)
		if !ok {
			return
		}
		for i, item := range items {
			walk(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case reflect.Map:
		m, ok := doc.(map[string]any)
		if !ok {
			return
		}
		for k, v := range m {
			walk(v, t.Elem(), join(path, k), errs) // any key is valid in a map
		}
	}
}

// jsonFields maps a struct's wire names to their field types, following
// embedded structs the way encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue // deliberately not on the wire (see wireExtras)
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// describe renders one error, suggesting the closest valid name when there is
// an obvious candidate — a typo'd field is far more common than an invented one.
func describe(path, key string, fields map[string]reflect.Type, extras map[string]bool) string {
	valid := make([]string, 0, len(fields)+len(extras))
	for k := range fields {
		valid = append(valid, k)
	}
	for k := range extras {
		valid = append(valid, k)
	}
	if best, ok := nearest(key, valid); ok {
		return fmt.Sprintf("%q (did you mean %q?)", path, best)
	}
	return fmt.Sprintf("%q", path)
}

// nearest returns the closest candidate within a small edit distance, scaled to
// the key's length so short names don't match everything.
func nearest(key string, candidates []string) (string, bool) {
	budget := len(key)/4 + 1
	if budget > 4 {
		budget = 4
	}
	best, bestD := "", budget+1
	for _, c := range candidates {
		if strings.EqualFold(c, key) {
			return c, true
		}
		if d := editDistance(strings.ToLower(key), strings.ToLower(c)); d < bestD {
			best, bestD = c, d
		}
	}
	if bestD <= budget {
		return best, true
	}
	return "", false
}

// editDistance is Levenshtein, iterative with two rows.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
