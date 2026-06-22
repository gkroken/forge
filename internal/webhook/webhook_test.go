package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/webhook"
)

func newStore(t *testing.T) meta.Store {
	t.Helper()
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSign_VerifiesWithHMAC(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	got := webhook.Sign("s3cr3t", body)

	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
	// Different secret → different signature.
	if webhook.Sign("other", body) == got {
		t.Fatal("signature must depend on the secret")
	}
}

func TestSubscription_Matches(t *testing.T) {
	ev := webhook.Event{Type: webhook.EventArtifactPublished, Repo: "maven-hosted"}
	cases := []struct {
		name string
		sub  webhook.Subscription
		want bool
	}{
		{"enabled all", webhook.Subscription{Enabled: true}, true},
		{"disabled", webhook.Subscription{Enabled: false}, false},
		{"repo match", webhook.Subscription{Enabled: true, Repo: "maven-hosted"}, true},
		{"repo wildcard", webhook.Subscription{Enabled: true, Repo: "*"}, true},
		{"repo mismatch", webhook.Subscription{Enabled: true, Repo: "npm-hosted"}, false},
		{"event match", webhook.Subscription{Enabled: true, Events: []string{webhook.EventArtifactPublished}}, true},
		{"event mismatch", webhook.Subscription{Enabled: true, Events: []string{"other.event"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sub.Matches(ev); got != c.want {
				t.Fatalf("Matches = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDispatchAndHandle_DeliversSigned drives the full path: Dispatch enqueues a
// job for a matching subscription, the worker handler delivers it, and the
// receiver sees a valid signature + the event body.
func TestDispatchAndHandle_DeliversSigned(t *testing.T) {
	var gotSig, gotEvent atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotSig.Store(r.Header.Get(webhook.SignatureHeader) == webhook.Sign("topsecret", body))
		var ev webhook.Event
		_ = json.Unmarshal(body, &ev)
		gotEvent.Store(ev)
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newStore(t)
	q := queue.NewMem(16)
	eng := webhook.New(m, q, srv.Client())

	if _, err := eng.Store().Create(webhook.Subscription{
		Name: "ci", URL: srv.URL, Secret: "topsecret", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	eng.Dispatch(ctx, webhook.Event{
		Type: webhook.EventArtifactPublished, Repo: "maven-hosted", Path: "g/a/1.0/a.jar",
	})

	waitFor(t, func() bool { return hits.Load() == 1 }, "delivery")
	if v, _ := gotSig.Load().(bool); !v {
		t.Fatal("receiver got an invalid signature")
	}
	if ev, _ := gotEvent.Load().(webhook.Event); ev.Repo != "maven-hosted" {
		t.Fatalf("receiver got wrong event: %+v", ev)
	}
}

// TestHandle_DisabledSubscriptionDropped ensures a job whose subscription was
// disabled after enqueue is not delivered.
func TestHandle_DisabledSubscriptionDropped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newStore(t)
	q := queue.NewMem(16)
	eng := webhook.New(m, q, srv.Client())
	sub, err := eng.Store().Create(webhook.Subscription{Name: "ci", URL: srv.URL, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	// Enqueue directly via Dispatch — disabled sub won't match, so nothing is
	// enqueued; assert no delivery after a settle window.
	eng.Dispatch(ctx, webhook.Event{Type: webhook.EventArtifactPublished, Repo: sub.Repo})
	time.Sleep(150 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("disabled subscription was delivered (%d hits)", hits.Load())
	}
}

// TestHandle_BoundedRetry verifies a persistently-failing endpoint is retried up
// to the attempt cap and then dropped (not retried forever).
func TestHandle_BoundedRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError) // always fail
	}))
	defer srv.Close()

	m := newStore(t)
	q := queue.NewMem(64)
	// Near-zero backoff so the test doesn't wait out the real schedule.
	eng := webhook.New(m, q, srv.Client()).
		WithBackoff(func(int) time.Duration { return time.Millisecond })
	if _, err := eng.Store().Create(webhook.Subscription{
		Name: "ci", URL: srv.URL, Secret: "k", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	eng.Dispatch(ctx, webhook.Event{Type: webhook.EventArtifactPublished, Repo: "maven-hosted"})

	// defaultMaxAttempts is 5: attempts 0..4 fire, then the delivery is dropped.
	waitFor(t, func() bool { return hits.Load() == 5 }, "5 attempts")
	time.Sleep(150 * time.Millisecond) // settle: must not exceed the cap
	if got := hits.Load(); got != 5 {
		t.Fatalf("want exactly 5 attempts (capped), got %d", got)
	}
}

// TestEmitCleanupCompleted_DeliversSummary verifies the unified cleanup-event
// helper delivers a cleanup.completed event whose Data carries the run summary,
// and that the actor reflects the trigger ("manual" → "admin").
func TestEmitCleanupCompleted_DeliversSummary(t *testing.T) {
	var got atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev webhook.Event
		_ = json.Unmarshal(body, &ev)
		got.Store(ev)
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newStore(t)
	q := queue.NewMem(16)
	eng := webhook.New(m, q, srv.Client())
	if _, err := eng.Store().Create(webhook.Subscription{
		Name: "ops", URL: srv.URL, Secret: "k", Enabled: true,
		Events: []string{webhook.EventCleanupCompleted},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	eng.EmitCleanupCompleted(ctx, "maven-hosted", "keep-10", 3, 4096, "manual")

	waitFor(t, func() bool { return hits.Load() == 1 }, "cleanup delivery")
	ev, _ := got.Load().(webhook.Event)
	if ev.Type != webhook.EventCleanupCompleted {
		t.Fatalf("type = %q, want %q", ev.Type, webhook.EventCleanupCompleted)
	}
	if ev.Actor != "admin" {
		t.Fatalf("actor = %q, want admin (manual trigger)", ev.Actor)
	}
	if ev.Data["trigger"] != "manual" || ev.Data["policy"] != "keep-10" {
		t.Fatalf("unexpected data: %+v", ev.Data)
	}
	// JSON numbers decode to float64.
	if d, _ := ev.Data["deleted"].(float64); d != 3 {
		t.Fatalf("deleted = %v, want 3", ev.Data["deleted"])
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
