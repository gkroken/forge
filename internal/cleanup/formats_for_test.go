package cleanup_test

import (
	"forge/internal/cleanup"
	formatsPkg "forge/internal/formats"
	"forge/internal/repo"
)

// formats is the real handler registry, so retention tests exercise the same
// dispatch production does rather than a stub that could drift from it.
func formats() cleanup.Resolver { return formatsPkg.Registry() }

// rp is shorthand for the repository value the retention entry points now take.
func rp(name, formatName string) repo.Repository {
	return repo.Repository{Name: name, Format: formatName, Kind: repo.Hosted, Enabled: true}
}
