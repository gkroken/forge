package cleanup_test

import (
	"forge/internal/cleanup"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/repo"
)

// formats is the real handler registry, so retention tests exercise the same
// dispatch production does rather than a stub that could drift from it.
func formats() cleanup.Resolver {
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())
	reg.Register(oci.New())
	return reg
}

// rp is shorthand for the repository value the retention entry points now take.
func rp(name, formatName string) repo.Repository {
	return repo.Repository{Name: name, Format: formatName, Kind: repo.Hosted, Enabled: true}
}
