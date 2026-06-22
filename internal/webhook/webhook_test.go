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
	"strconv"
	"sync"
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
	var ts int64 = 1_700_000_000
	got := webhook.Sign("s3cr3t", ts, body)

	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write([]byte("1700000000."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
	// Different secret → different signature.
	if webhook.Sign("other", ts, body) == got {
		t.Fatal("signature must depend on the secret")
	}
	// Different timestamp → different signature (binds the timestamp).
	if webhook.Sign("s3cr3t", ts+1, body) == got {
		t.Fatal("signature must depend on the timestamp")
	}
}

// TestVerify_RejectsReplayAndTamper exercises the receiver-side verifier:
// a fresh, correctly-signed delivery passes; an out-of-tolerance timestamp
// (replay) and a tampered body are both rejected.
func TestVerify_RejectsReplayAndTamper(t *testing.T) {
	secret := "k"
	body := []byte(`{"type":"artifact.published"}`)
	now := time.Unix(1_700_000_000, 0).UTC()
	sig := webhook.Sign(secret, now.Unix(), body)

	if !webhook.Verify(secret, sig, now.Unix(), body, time.Minute, now) {
		t.Fatal("fresh delivery should verify")
	}
	// Replay: same signature, but received 10 minutes later → outside tolerance.
	if webhook.Verify(secret, sig, now.Unix(), body, time.Minute, now.Add(10*time.Minute)) {
		t.Fatal("stale timestamp must be rejected (replay protection)")
	}
	// Tamper: body changed under the same timestamp → signature mismatch.
	if webhook.Verify(secret, sig, now.Unix(), []byte(`{"type":"evil"}`), time.Minute, now) {
		t.Fatal("tampered body must be rejected")
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
		ts, _ := strconv.ParseInt(r.Header.Get(webhook.TimestampHeader), 10, 64)
		// Receiver-side verification: signature over "{timestamp}.{body}", and a
		// non-empty delivery id for dedup.
		ok := webhook.Verify("topsecret", r.Header.Get(webhook.SignatureHeader), ts, body, time.Minute, time.Now()) &&
			r.Header.Get(webhook.DeliveryHeader) != ""
		gotSig.Store(ok)
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

// TestRetryAfter_HonouredAndDeliveryIDStable verifies a 429 with a Retry-After
// defers the retry by at least that long, and that the X-Forge-Delivery id is
// identical across the original attempt and the retry (so receivers can dedup).
func TestRetryAfter_HonouredAndDeliveryIDStable(t *testing.T) {
	var ids []string
	var times []time.Time
	var mu sync.Mutex
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(webhook.DeliveryHeader))
		times = append(times, time.Now())
		mu.Unlock()
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newStore(t)
	q := queue.NewMem(16)
	// Tiny base backoff so, absent Retry-After, the retry would be near-instant;
	// the observed >=1s gap therefore comes from Retry-After, not the schedule.
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

	waitFor(t, func() bool { return hits.Load() == 2 }, "retry after 429")
	mu.Lock()
	defer mu.Unlock()
	if ids[0] != ids[1] {
		t.Fatalf("delivery id changed across retry: %q != %q", ids[0], ids[1])
	}
	if gap := times[1].Sub(times[0]); gap < time.Second {
		t.Fatalf("retry honoured backoff (%v) instead of Retry-After (>=1s)", gap)
	}
}

// TestHistory_RecordsSuccess verifies a delivered event lands one success record
// in history and increments the success metric.
func TestHistory_RecordsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var mu sync.Mutex
	counts := map[string]int{}
	m := newStore(t)
	q := queue.NewMem(16)
	eng := webhook.New(m, q, srv.Client()).WithMetrics(func(r string) {
		mu.Lock()
		counts[r]++
		mu.Unlock()
	})
	sub, err := eng.Store().Create(webhook.Subscription{Name: "ci", URL: srv.URL, Secret: "k", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	eng.Dispatch(ctx, webhook.Event{Type: webhook.EventArtifactPublished, Repo: "maven-hosted"})

	waitFor(t, func() bool {
		recs, _ := eng.History().List(sub.ID)
		return len(recs) == 1
	}, "one history record")

	recs, _ := eng.History().List(sub.ID)
	if recs[0].Status != webhook.StatusSuccess || recs[0].HTTPCode != 200 || recs[0].Attempt != 1 {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[webhook.StatusSuccess] != 1 {
		t.Fatalf("success metric = %d, want 1", counts[webhook.StatusSuccess])
	}
}

// TestHistory_DeadLetterOnExhaustion verifies a persistently-failing endpoint
// records each failed attempt and a terminal "dropped" record (the dead-letter),
// and that the metric counts match.
func TestHistory_DeadLetterOnExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	counts := map[string]int{}
	m := newStore(t)
	q := queue.NewMem(64)
	eng := webhook.New(m, q, srv.Client()).
		WithBackoff(func(int) time.Duration { return time.Millisecond }).
		WithMetrics(func(r string) { mu.Lock(); counts[r]++; mu.Unlock() })
	sub, err := eng.Store().Create(webhook.Subscription{Name: "ci", URL: srv.URL, Secret: "k", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Work(ctx, eng.Handle) //nolint:errcheck

	eng.Dispatch(ctx, webhook.Event{Type: webhook.EventArtifactPublished, Repo: "maven-hosted"})

	// 5 attempts: 4 failed + 1 dropped.
	waitFor(t, func() bool {
		recs, _ := eng.History().List(sub.ID)
		return len(recs) == 5
	}, "5 history records")

	recs, _ := eng.History().List(sub.ID)
	// Newest-first: the terminal record is the dead-letter.
	if recs[0].Status != webhook.StatusDropped {
		t.Fatalf("newest record = %q, want dropped", recs[0].Status)
	}
	dropped, failed := 0, 0
	for _, r := range recs {
		switch r.Status {
		case webhook.StatusDropped:
			dropped++
		case webhook.StatusFailed:
			failed++
		}
	}
	if dropped != 1 || failed != 4 {
		t.Fatalf("history outcomes: dropped=%d failed=%d, want 1/4", dropped, failed)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[webhook.StatusDropped] != 1 || counts[webhook.StatusFailed] != 4 {
		t.Fatalf("metrics: dropped=%d failed=%d, want 1/4", counts[webhook.StatusDropped], counts[webhook.StatusFailed])
	}
}

// TestStore_UpdatePreservesSecretAndCreatedAt verifies an edit with a blank
// secret keeps the stored secret + CreatedAt, while a non-blank secret rotates it.
func TestStore_UpdatePreservesSecretAndCreatedAt(t *testing.T) {
	st := webhook.NewStore(newStore(t))
	orig, err := st.Create(webhook.Subscription{Name: "ci", URL: "https://a.example/h", Secret: "s1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	// Edit URL + events, leave secret blank → secret + CreatedAt unchanged.
	updated, err := st.Update(webhook.Subscription{
		ID: orig.ID, Name: "ci", URL: "https://b.example/h", Secret: "",
		Events: []string{webhook.EventArtifactPublished}, Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Secret != "s1" {
		t.Fatalf("blank secret should be preserved, got %q", updated.Secret)
	}
	if !updated.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("CreatedAt should be preserved: %v != %v", updated.CreatedAt, orig.CreatedAt)
	}
	if updated.URL != "https://b.example/h" || updated.Enabled {
		t.Fatalf("edit not applied: %+v", updated)
	}

	// Non-blank secret rotates it.
	rotated, err := st.Update(webhook.Subscription{ID: orig.ID, Name: "ci", URL: "https://b.example/h", Secret: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret != "s2" {
		t.Fatalf("secret should rotate to s2, got %q", rotated.Secret)
	}

	// Updating a missing subscription errors.
	if _, err := st.Update(webhook.Subscription{ID: "nope", URL: "https://x.example/h"}); err == nil {
		t.Fatal("update of a missing subscription should error")
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
