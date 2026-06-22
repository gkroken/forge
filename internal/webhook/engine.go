package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"forge/internal/meta"
	"forge/internal/queue"
)

// JobType is the queue job type for a single webhook delivery. It is registered
// as a handler on the shared async worker (see indexer.Worker.Register).
const JobType = "webhook.deliver"

// defaultMaxAttempts bounds delivery retries before a delivery is dropped.
const defaultMaxAttempts = 5

// Retry backoff: delayed re-enqueue (EnqueueAfter) so a temporarily-unavailable
// endpoint gets time to recover and the single worker isn't pinned on a doomed
// delivery. Equal-jitter exponential: backoffBase·2^(n-1), half-fixed plus
// random half to de-synchronise herds of failing deliveries, capped at maxBackoff.
const (
	backoffBase = 2 * time.Second
	maxBackoff  = 5 * time.Minute
)

// expBackoff returns the delay before retry attempt n (n >= 1).
func expBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase << (attempt - 1) // base * 2^(attempt-1)
	if d <= 0 || d > maxBackoff {      // overflow or past the ceiling
		d = maxBackoff
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// delivery is the enqueued job payload. It carries the subscription ID (not the
// secret) so the secret never lands in the queue table and a since-disabled or
// deleted subscription is honoured at delivery time.
type delivery struct {
	SubID   string `json:"subID"`
	Event   Event  `json:"event"`
	Attempt int    `json:"attempt"`
}

// Engine matches events to subscriptions, enqueues durable delivery jobs, and
// delivers them (as the worker handler). One per server.
type Engine struct {
	store       *Store
	q           queue.Queue
	client      *http.Client
	maxAttempts int
	backoff     func(attempt int) time.Duration
}

// New returns an Engine persisting subscriptions in m and delivering via q.
// A nil client gets a 10s-timeout default.
func New(m meta.Store, q queue.Queue, client *http.Client) *Engine {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Engine{
		store: NewStore(m), q: q, client: client,
		maxAttempts: defaultMaxAttempts,
		backoff:     expBackoff,
	}
}

// WithBackoff overrides the retry backoff schedule (delay before retry attempt
// n). Used by tests to avoid real waits; returns e for chaining.
func (e *Engine) WithBackoff(fn func(attempt int) time.Duration) *Engine {
	e.backoff = fn
	return e
}

// Store exposes the subscription store for the admin API/UI.
func (e *Engine) Store() *Store { return e.store }

// Dispatch enqueues one delivery job per enabled subscription matching ev. It is
// best-effort and non-blocking past the enqueue: errors are logged, not returned,
// so a publish is never failed by webhook plumbing.
func (e *Engine) Dispatch(ctx context.Context, ev Event) {
	if e.q == nil {
		return
	}
	subs, err := e.store.List()
	if err != nil {
		slog.Warn("webhook: list subscriptions failed", "err", err)
		return
	}
	for _, s := range subs {
		if !s.Matches(ev) {
			continue
		}
		if err := e.q.Enqueue(ctx, JobType, delivery{SubID: s.ID, Event: ev}); err != nil {
			slog.Warn("webhook: enqueue delivery failed", "sub", s.ID, "err", err)
		}
	}
}

// Handle is the worker handler for JobType jobs. It loads the subscription and
// POSTs the signed event, with bounded self-managed retry. It always returns nil
// so the queue's generic immediate-retry does not double-fire; transient store
// errors are the one exception (returned so the queue retries the load).
func (e *Engine) Handle(ctx context.Context, j queue.Job) error {
	var d delivery
	if err := j.UnmarshalPayload(&d); err != nil {
		slog.Warn("webhook: bad delivery payload", "err", err)
		return nil // poison message; never retry
	}
	sub, ok, err := e.store.Get(d.SubID)
	if err != nil {
		return err // transient store error → let the queue retry the load
	}
	if !ok || !sub.Enabled {
		return nil // subscription deleted/disabled since enqueue; drop
	}
	if derr := e.Deliver(ctx, sub, d.Event); derr != nil {
		if d.Attempt+1 < e.maxAttempts {
			d.Attempt++
			delay := e.backoff(d.Attempt)
			if eqErr := e.q.EnqueueAfter(ctx, JobType, d, delay); eqErr != nil {
				slog.Warn("webhook: re-enqueue failed", "sub", sub.ID, "err", eqErr)
			}
		} else {
			slog.Warn("webhook: delivery dropped after max attempts",
				"sub", sub.ID, "url", sub.URL, "attempts", e.maxAttempts, "err", derr)
		}
	}
	return nil
}

// Deliver POSTs ev to sub.URL as signed JSON. Returns an error on transport
// failure or a non-2xx response. Exported so an admin "test ping" can reuse it.
func (e *Engine) Deliver(ctx context.Context, sub Subscription, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, ev.Type)
	req.Header.Set(SignatureHeader, Sign(sub.Secret, body))

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: non-2xx from %s: %d", sub.URL, resp.StatusCode)
	}
	return nil
}
