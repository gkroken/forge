package testutil_test

import (
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/testutil"
)

// This reproduces the CI failure deterministically. A worker that keeps writing
// for a moment after being asked to stop will race t.TempDir's RemoveAll, and
// the test fails with "unlinkat ...: directory not empty" — which is what took
// down two of four matrix entries while passing 20 local runs in a row.
//
// The inner subtest is where the temp directory and the worker live, so its
// cleanups run while the outer test is still able to observe the result.
func TestRunWorker_JoinsBeforeTempDirRemoval(t *testing.T) {
	var wrote atomic.Int64
	ok := t.Run("inner", func(t *testing.T) {
		dir := t.TempDir()
		stop := make(chan struct{})
		testutil.RunWorker(t, func() { close(stop) }, func() {
			<-stop
			// Still busy after the stop signal — exactly the situation that
			// makes signalling insufficient.
			for i := 0; i < 200; i++ {
				name := filepath.Join(dir, strconv.Itoa(i))
				if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
					return
				}
				wrote.Add(1)
				time.Sleep(time.Millisecond)
			}
		})
	})
	if !ok {
		t.Error("the subtest failed: its temp directory was removed while the " +
			"worker was still writing, which is the flake this helper exists to prevent")
	}
	if wrote.Load() == 0 {
		t.Error("the worker never ran, so this proved nothing")
	}
}

// And the plain guarantee: RunWorker does not let the test finish until the
// worker has actually returned.
func TestRunWorker_WaitsForCompletion(t *testing.T) {
	var finished atomic.Bool
	t.Run("inner", func(t *testing.T) {
		stop := make(chan struct{})
		testutil.RunWorker(t, func() { close(stop) }, func() {
			<-stop
			time.Sleep(50 * time.Millisecond)
			finished.Store(true)
		})
	})
	if !finished.Load() {
		t.Error("the subtest returned before its worker had stopped")
	}
}
