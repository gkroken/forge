package selector

import "testing"

func TestValidate(t *testing.T) {
	valid := []string{
		"com/acme/**",
		"@acme/**",
		"acme-*",
		"com/acme/*/1.0/**",
		"src/contrib/acme*",
		"a",
		"**",
	}
	for _, p := range valid {
		if err := Validate(p); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{
		"",
		"/com/acme",
		"com/acme/",
		"com//acme",
		"a**b",
		"com/a**/x",
	}
	for _, p := range invalid {
		if err := Validate(p); err == nil {
			t.Errorf("Validate(%q) = nil, want error", p)
		}
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		// Maven groupId scoping
		{"com/acme/**", "com/acme/app/1.0/app-1.0.jar", true},
		{"com/acme/**", "com/acme", true}, // ** matches zero segments
		{"com/acme/**", "com/acmeco/app/1.0/app.jar", false},
		{"com/acme/**", "org/acme/app/1.0/app.jar", false},
		{"com/acme/**", "com/acme/maven-metadata.xml", true},

		// npm scope
		{"@acme/**", "@acme/ui", true},
		{"@acme/**", "@acme/ui/-/ui-1.0.0.tgz", true},
		{"@acme/**", "@acmex/ui", false},
		{"@acme/**", "lodash", false},
		{"@acme/**", "lodash/-/lodash-4.17.21.tgz", false},

		// single-star within a segment
		{"acme-*", "acme-charts", true},
		{"acme-*", "acme-", true},
		{"acme-*", "acme-a/b", false}, // * does not cross /
		{"acme-*/**", "acme-a/b", true},
		{"*", "anything", true},
		{"*", "a/b", false},

		// ** in the middle
		{"com/**/app.jar", "com/a/b/app.jar", true},
		{"com/**/app.jar", "com/app.jar", true},
		{"com/**/app.jar", "org/a/app.jar", false},

		// multiple stars in one segment
		{"*-SNAPSHOT*", "1.0-SNAPSHOT.jar", true},
		{"*-SNAPSHOT*", "1.0.jar", false},

		// leading slash on path tolerated
		{"com/acme/**", "/com/acme/app.jar", true},

		// exactness
		{"com/acme", "com/acme", true},
		{"com/acme", "com/acme/x", false},

		// empty / malformed fail closed
		{"", "anything", false},
		{"a**b", "aXb", false},
		{"com/acme/**", "", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestMatchAny(t *testing.T) {
	pats := []string{"com/acme/**", "@acme/**"}
	if !MatchAny(pats, "@acme/ui") {
		t.Error("MatchAny should match second pattern")
	}
	if MatchAny(pats, "org/other/x.jar") {
		t.Error("MatchAny should not match")
	}
	if MatchAny(nil, "anything") {
		t.Error("MatchAny(nil) should be false")
	}
}

// FuzzMatch asserts Match never panics and terminates on arbitrary input.
func FuzzMatch(f *testing.F) {
	f.Add("com/acme/**", "com/acme/app.jar")
	f.Add("**/**/**", "a/b/c/d/e/f")
	f.Add("a*b*c", "abc")
	f.Fuzz(func(t *testing.T, pattern, path string) {
		Match(pattern, path)
	})
}
