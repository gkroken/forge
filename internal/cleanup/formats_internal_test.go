package cleanup

import (
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
)

// testFormats is the real handler registry for in-package tests, so they
// dispatch exactly as production does.
func testFormats() Resolver {
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())
	reg.Register(oci.New())
	return reg
}
