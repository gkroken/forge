package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/queue"
	"forge/internal/testutil"
)

func newMetaStore(t *testing.T) meta.Store {
	t.Helper()
	m, err := meta.NewFS(filepath.Join(t.TempDir(), "m"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRegenNPM_Idempotent verifies that running the same regen job multiple
// times produces the same packument.
func TestRegenNPM_Idempotent(t *testing.T) {
	m := newMetaStore(t)
	w := New(m)

	// Store two versions.
	m.PutJSON("repo1:npm:v", "mypkg:1.0.0", map[string]any{"name": "mypkg", "version": "1.0.0"})
	m.PutJSON("repo1:npm:v", "mypkg:2.0.0", map[string]any{"name": "mypkg", "version": "2.0.0"})
	m.PutJSON("repo1:npm:dt", "mypkg", map[string]any{"latest": "2.0.0"})

	payload, _ := json.Marshal(RegenPayload{RepoName: "repo1", Pkg: "mypkg"})
	j := queue.Job{ID: "1", Type: "npm.regen", Payload: payload}

	// Run regen twice; both must produce the same packument.
	for i := 0; i < 2; i++ {
		if err := w.regenNPM(j); err != nil {
			t.Fatalf("regen %d: %v", i, err)
		}
	}

	var packument map[string]any
	if ok, _ := m.GetJSON("repo1:npm", "mypkg", &packument); !ok {
		t.Fatal("packument not written")
	}
	versions, _ := packument["versions"].(map[string]any)
	if len(versions) != 2 {
		t.Errorf("expected 2 versions, got %d", len(versions))
	}
	dt, _ := packument["dist-tags"].(map[string]any)
	if dt["latest"] != "2.0.0" {
		t.Errorf("expected dist-tag latest=2.0.0, got %v", dt["latest"])
	}
}

// TestRegenNPM_NoOpWhenEmpty verifies that regen is a no-op when no
// per-version records exist (old-format packuments are left untouched).
func TestRegenNPM_NoOpWhenEmpty(t *testing.T) {
	m := newMetaStore(t)
	w := New(m)

	// Seed an old-format packument (no per-version records).
	m.PutJSON("repo1:npm", "mypkg", map[string]any{
		"name": "mypkg",
		"versions": map[string]any{
			"1.0.0": map[string]any{"name": "mypkg", "version": "1.0.0"},
		},
	})

	payload, _ := json.Marshal(RegenPayload{RepoName: "repo1", Pkg: "mypkg"})
	j := queue.Job{ID: "1", Type: "npm.regen", Payload: payload}

	if err := w.regenNPM(j); err != nil {
		t.Fatal(err)
	}

	// Old-format packument must be untouched.
	var packument map[string]any
	m.GetJSON("repo1:npm", "mypkg", &packument)
	versions, _ := packument["versions"].(map[string]any)
	if _, ok := versions["1.0.0"]; !ok {
		t.Error("expected old-format packument to survive regen no-op")
	}
}

// TestConcurrentPublish is the core P5 correctness test.
//
// 20 goroutines simultaneously publish different versions of the same package.
// Each publish writes to a unique per-version key so there are no write
// conflicts.  After the queue drains, the rebuilt packument must contain
// all 20 versions — proving no lost updates.
func TestConcurrentPublish(t *testing.T) {
	const N = 20
	m := newMetaStore(t)
	q := queue.NewMem(N * 2)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the indexer worker.
	testutil.RunWorker(t, cancel, func() { New(m).Work(ctx, q) }) //nolint:errcheck

	// Publish N versions concurrently.
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ver := fmt.Sprintf("1.%d.0", i)
			vobj := map[string]any{
				"name":    "concurrent-pkg",
				"version": ver,
				"dist":    map[string]any{"tarball": "http://localhost/concurrent-pkg/-/" + ver + ".tgz"},
			}
			// Each version is a unique key — no conflicts between goroutines.
			m.PutJSON("r:npm:v", "concurrent-pkg:"+ver, vobj) //nolint:errcheck
			// Enqueue the regen job (deduplicated by the idempotent worker).
			q.Enqueue(ctx, "npm.regen", RegenPayload{ //nolint:errcheck
				RepoName: "r",
				Pkg:      "concurrent-pkg",
			})
		}(i)
	}
	wg.Wait() // all publishes done
	q.Drain() // all regen jobs processed

	// Every version must appear in the final packument.
	var packument map[string]any
	if ok, _ := m.GetJSON("r:npm", "concurrent-pkg", &packument); !ok {
		t.Fatal("packument not found after concurrent publishes")
	}
	versions, _ := packument["versions"].(map[string]any)
	if len(versions) != N {
		t.Errorf("expected %d versions in packument, got %d (lost updates!)", N, len(versions))
		for ver := range versions {
			t.Logf("  present: %s", ver)
		}
	}
}

// TestWorker_WithMetrics covers WithMetrics and the metrics-recording branch
// inside Work. A real npm.regen job is enqueued and drained so the success
// counter path (w.metrics != nil) is exercised.
func TestWorker_WithMetrics(t *testing.T) {
	m := newMetaStore(t)
	m.PutJSON("repo1:npm:v", "pkg:1.0.0", map[string]any{"name": "pkg", "version": "1.0.0"})
	m.PutJSON("repo1:npm:dt", "pkg", map[string]any{"latest": "1.0.0"})

	metrics := obs.NewMetrics(prometheus.NewRegistry())
	q := queue.NewMem(4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testutil.RunWorker(t, cancel, func() { New(m).WithMetrics(metrics).Work(ctx, q) }) //nolint:errcheck

	payload, _ := json.Marshal(RegenPayload{RepoName: "repo1", Pkg: "pkg"})
	q.Enqueue(ctx, "npm.regen", RegenPayload{RepoName: "repo1", Pkg: "pkg"}) //nolint:errcheck
	_ = payload
	q.Drain()
}

// TestRegister_CustomHandlerDispatched verifies a non-index job family attaches
// via Register and is drained by this one worker alongside npm.regen — the seam
// that lets webhook delivery share the queue instead of racing a 2nd worker.
func TestRegister_CustomHandlerDispatched(t *testing.T) {
	m := newMetaStore(t)
	q := queue.NewMem(4)

	var got queue.Job
	done := make(chan struct{})
	w := New(m).Register("webhook.deliver", func(_ context.Context, j queue.Job) error {
		got = j
		close(done)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testutil.RunWorker(t, cancel, func() { w.Work(ctx, q) }) //nolint:errcheck

	q.Enqueue(ctx, "webhook.deliver", map[string]string{"subID": "s1"}) //nolint:errcheck
	q.Drain()
	<-done

	if got.Type != "webhook.deliver" {
		t.Errorf("registered handler got job type %q, want webhook.deliver", got.Type)
	}
}

// TestDispatch_RegisteredHandlerWins verifies a registered type is routed to its
// handler (not the built-in switch) and its error propagates, while an unknown
// type is silently discarded (returns nil) rather than erroring the queue.
func TestDispatch_RegisteredHandlerWins(t *testing.T) {
	m := newMetaStore(t)
	wantErr := fmt.Errorf("boom")
	w := New(m).Register("custom.job", func(_ context.Context, _ queue.Job) error {
		return wantErr
	})

	if err := w.dispatch(context.Background(), queue.Job{Type: "custom.job"}); err != wantErr {
		t.Errorf("dispatch(custom.job) err = %v, want %v", err, wantErr)
	}
	if err := w.dispatch(context.Background(), queue.Job{Type: "totally.unknown"}); err != nil {
		t.Errorf("dispatch(unknown) err = %v, want nil (discarded)", err)
	}
}

// TestWithTaskRing_RecordsCompletion verifies WithTaskRing wires the ring so a
// drained job surfaces as a completed task in the system tasks API.
func TestWithTaskRing_RecordsCompletion(t *testing.T) {
	m := newMetaStore(t)
	m.PutJSON("repo1:npm:v", "pkg:1.0.0", map[string]any{"name": "pkg", "version": "1.0.0"})
	m.PutJSON("repo1:npm:dt", "pkg", map[string]any{"latest": "1.0.0"})

	ring := queue.NewTaskRing(8)
	q := queue.NewMem(4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testutil.RunWorker(t, cancel, func() { New(m).WithTaskRing(ring).Work(ctx, q) }) //nolint:errcheck

	q.Enqueue(ctx, "npm.regen", RegenPayload{RepoName: "repo1", Pkg: "pkg"}) //nolint:errcheck
	q.Drain()

	recent := ring.Recent(4)
	var found bool
	for _, ti := range recent {
		if ti.Name == "npm.regen" && ti.Status == "done" {
			found = true
		}
	}
	if !found {
		t.Errorf("task ring did not record a done npm.regen task; got %+v", recent)
	}
}
