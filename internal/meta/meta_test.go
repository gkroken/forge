package meta_test

import (
	"testing"

	"forge/internal/meta"
	"forge/internal/meta/metatest"
)

// The FS store must satisfy the same contract every backend will.
func TestFS_Contract(t *testing.T) {
	s, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metatest.RunContract(t, s)
}
