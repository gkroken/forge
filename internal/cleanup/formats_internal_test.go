package cleanup

import (
	"forge/internal/formats"
)

// testFormats is the real handler registry for in-package tests, so they
// dispatch exactly as production does.
func testFormats() Resolver { return formats.Registry() }
