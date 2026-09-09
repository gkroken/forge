package config_test

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/config"
	"forge/internal/repo"
	"forge/internal/testutil"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatch_ReAppliesOnFileChange(t *testing.T) {
	a := newAppliers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.config.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repositories:\n  - name: one\n    format: npm\n    kind: hosted\n    enabled: true\n")
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	testutil.RunWorker(t, func() { close(done) }, func() { config.Watch(path, a, 10*time.Millisecond, done, func(config.Result, error) {}) })

	// A new repo appears in the file → it must appear in the manager.
	write("repositories:\n  - name: one\n    format: npm\n    kind: hosted\n    enabled: true\n" +
		"  - name: two\n    format: npm\n    kind: hosted\n    enabled: true\n")
	waitFor(t, 2*time.Second, "repo two to be applied", func() bool {
		_, ok := a.Repos.Get("two")
		return ok
	})
}

// TestWatch_IgnoresLiveStateChanges is the recorded design decision: the
// watcher reconciles on FILE changes only. Reverting an out-of-band edit on a
// timer is self-heal, which forge does not do.
func TestWatch_IgnoresLiveStateChanges(t *testing.T) {
	a := newAppliers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.config.yaml")
	if err := os.WriteFile(path,
		[]byte("repositories:\n  - name: one\n    format: npm\n    kind: hosted\n    enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply(f, a); err != nil {
		t.Fatal(err)
	}

	var passes atomic.Int64
	done := make(chan struct{})
	testutil.RunWorker(t, func() { close(done) }, func() { config.Watch(path, a, 10*time.Millisecond, done, func(config.Result, error) { passes.Add(1) }) })

	// Let the watcher take its first pass (which applies once by design, so a
	// change landing between the boot apply and the watcher starting is never
	// missed). Steady state begins after that.
	waitFor(t, 2*time.Second, "the watcher's first pass", func() bool { return passes.Load() > 0 })

	// Now a break-glass edit, in steady state. The file is untouched, so the
	// watcher must leave it alone: reverting it would be self-heal.
	rp, _ := a.Repos.Get("one")
	rp.AnonymousRead = true
	if err := a.Repos.Update(rp); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	got, _ := a.Repos.Get("one")
	if !got.AnonymousRead {
		t.Error("watcher reverted a live edit — that is self-heal, which is out of scope by design")
	}
}

// TestWatch_RetriesAfterBadWrite — a half-written file must not be swallowed;
// the next good read has to apply.
func TestWatch_RetriesAfterBadWrite(t *testing.T) {
	a := newAppliers(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.config.yaml")
	if err := os.WriteFile(path, []byte("repositories: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errs atomic.Int64
	done := make(chan struct{})
	testutil.RunWorker(t, func() { close(done) }, func() {
		config.Watch(path, a, 10*time.Millisecond, done, func(_ config.Result, err error) {
			if err != nil {
				errs.Add(1)
			}
		})
	})

	// Broken YAML → reported, not applied.
	if err := os.WriteFile(path, []byte("repositories:\n  - name: x\n   bad: indent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "a parse error to be reported", func() bool { return errs.Load() > 0 })

	// Fixed → applied.
	if err := os.WriteFile(path,
		[]byte("repositories:\n  - name: fixed\n    format: npm\n    kind: hosted\n    enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "the corrected file to apply", func() bool {
		_, ok := a.Repos.Get("fixed")
		return ok
	})
}

// TestWatch_StopsOnDone — no goroutine left running after shutdown.
func TestWatch_StopsOnDone(t *testing.T) {
	a := newAppliers(t)
	path := filepath.Join(t.TempDir(), "forge.config.yaml")
	if err := os.WriteFile(path, []byte("repositories: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		config.Watch(path, a, 5*time.Millisecond, done, func(config.Result, error) {})
		close(stopped)
	}()
	close(done)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after done was closed")
	}
}

var _ = repo.Repository{} // keep the import honest if the file is trimmed
