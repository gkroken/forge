package testutil

import (
	"testing"
	"time"
)

// RunWorker starts a background worker and guarantees it has stopped before the
// test's temporary directories are removed.
//
// Signalling a worker to stop is not the same as waiting for it. Tests here
// started a config watcher or a queue worker, then closed a channel or called
// cancel() on the way out — which only asks it to stop. If the worker was
// mid-write when the test returned, t.TempDir's cleanup ran RemoveAll against a
// directory the worker was still filling and failed the test with
// "unlinkat ...: directory not empty".
//
// That is a flake, not a bug in the code under test, and it only appears under
// load: it failed two of four CI matrix entries while passing locally 20 runs
// in a row. A flaky suite is worse than a missing one, because it teaches
// everybody to re-run red builds.
//
// Ordering works because cleanups run last-registered-first: t.TempDir
// registers its removal when it is called, and this registers later, so the
// worker is joined before the directory goes away.
func RunWorker(t *testing.T, stop func(), run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("background worker did not stop after being signalled; " +
				"it may still be writing to a temp directory")
		}
	})
}
