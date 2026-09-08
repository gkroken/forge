// Package formats is the single place forge decides which package formats it
// serves.
//
// It exists because there used to be three lists: main.go's registrations and
// two hand-built registries inside tests. The roll-call tests that are supposed
// to catch a half-wired format were reading their own list, so registering a new
// format in main.go left them green — the exact failure they exist to prevent,
// occurring in the guard itself. Found while adding pypi.
package formats

import (
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/format/pypi"
)

// All returns every handler this build serves. Adding a format means adding it
// here, and nowhere else.
func All() []format.Handler {
	return []format.Handler{
		maven.New(),
		npm.New(),
		helm.New(),
		cran.New(),
		oci.New(),
		pypi.New(),
	}
}

// Registry returns All() registered and ready to dispatch.
func Registry() *format.Registry {
	reg := format.NewRegistry()
	for _, h := range All() {
		reg.Register(h)
	}
	return reg
}
