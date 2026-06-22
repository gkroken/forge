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
	"strconv"
	"strings"
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

// SchemaVersion is the delivered payload's envelope version. Bumped to 2 when
// the signature contract changed to sign "{timestamp}.{body}" and the envelope
// gained id + schemaVersion fields.
const SchemaVersion = 2

// delivery is the enqueued job payload. It carries the subscription ID (not the
// secret) so the secret never lands in the queue table and a since-disabled or
// deleted subscription is honoured at delivery time. DeliveryID is generated
// once at Dispatch and preserved across retries so a receiver can dedup.
type delivery struct {
	SubID      string `json:"subID"`
	DeliveryID string `json:"deliveryID"`
	Event      Event  `json:"event"`
	Attempt    int    `json:"attempt"`
}

// deliveryPayload is the JSON body POSTed to a subscriber: the event fields
// flattened under a delivery envelope (schemaVersion + stable id). The id also
// travels in the X-Forge-Delivery header.
type deliveryPayload struct {
	SchemaVersion int    `json:"schemaVersion"`
	ID            string `json:"id"`
	Event
}

// Engine matches events to subscriptions, enqueues durable delivery jobs, and
// delivers them (as the worker handler). One per server.
type Engine struct {
	store       *Store
	history     *History
	q           queue.Queue
	client      *http.Client
	maxAttempts int
	backoff     func(attempt int) time.Duration
	metricsFn   func(result string) // optional; records the delivery outcome
}

// New returns an Engine persisting subscriptions in m and delivering via q.
// A nil client gets a 10s-timeout default.
func New(m meta.Store, q queue.Queue, client *http.Client) *Engine {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Engine{
		store: NewStore(m), history: NewHistory(m), q: q, client: client,
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

// WithMetrics registers a callback invoked with each delivery outcome
// ("success"/"failed"/"dropped") so the server can increment a Prometheus
// counter without the webhook package depending on obs. Returns e for chaining.
func (e *Engine) WithMetrics(fn func(result string)) *Engine {
	e.metricsFn = fn
	return e
}

// Store exposes the subscription store for the admin API/UI.
func (e *Engine) Store() *Store { return e.store }

// History exposes the delivery-history store for the admin API/UI.
func (e *Engine) History() *History { return e.history }

// record persists a delivery outcome to history and increments the metric.
func (e *Engine) record(rec DeliveryRecord) {
	e.history.Append(rec)
	if e.metricsFn != nil {
		e.metricsFn(rec.Status)
	}
}

// EmitCleanupCompleted dispatches a cleanup.completed event for a run that
// removed at least one artifact. It unifies payload construction across the
// automated scheduler hook (trigger "scheduled"/"on-publish") and the manual
// admin handlers (trigger "manual"), so the event shape is identical whatever
// triggered the run. Actor is derived from the trigger.
func (e *Engine) EmitCleanupCompleted(ctx context.Context, repo, policy string, deleted int, freedBytes int64, trigger string) {
	actor := "scheduler"
	if trigger == "manual" {
		actor = "admin"
	}
	e.Dispatch(ctx, Event{
		Type:      EventCleanupCompleted,
		Repo:      repo,
		Actor:     actor,
		Timestamp: time.Now().UTC(),
		Data: map[string]any{
			"policy": policy, "deleted": deleted,
			"freedBytes": freedBytes, "trigger": trigger,
		},
	})
}

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
		d := delivery{SubID: s.ID, DeliveryID: NewID(), Event: ev}
		if err := e.q.Enqueue(ctx, JobType, d); err != nil {
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
	res, derr := e.Deliver(ctx, sub, d.Event, d.DeliveryID)
	attempt := d.Attempt + 1 // 1-based for the record
	rec := DeliveryRecord{
		ID: d.DeliveryID, SubID: sub.ID, Event: d.Event.Type, Repo: d.Event.Repo,
		HTTPCode: res.StatusCode, Attempt: attempt, Timestamp: time.Now().UTC(),
	}
	if derr == nil {
		rec.Status = StatusSuccess
		e.record(rec)
		return nil
	}
	rec.Error = derr.Error()
	if d.Attempt+1 < e.maxAttempts {
		rec.Status = StatusFailed
		d.Attempt++
		delay := e.backoff(d.Attempt)
		// Honour a server-provided Retry-After (429/503): wait at least that
		// long, clamped so a hostile/huge value can't pin a job for days.
		retryAfter := res.RetryAfter
		if retryAfter > maxRetryAfter {
			retryAfter = maxRetryAfter
		}
		if retryAfter > delay {
			delay = retryAfter
		}
		if eqErr := e.q.EnqueueAfter(ctx, JobType, d, delay); eqErr != nil {
			slog.Warn("webhook: re-enqueue failed", "sub", sub.ID, "err", eqErr)
		}
	} else {
		rec.Status = StatusDropped
		slog.Warn("webhook: delivery dropped after max attempts",
			"sub", sub.ID, "url", sub.URL, "attempts", e.maxAttempts, "err", derr)
	}
	e.record(rec)
	return nil
}

// maxRetryAfter caps how long a Retry-After header can defer a redelivery.
const maxRetryAfter = time.Hour

// DeliverResult carries the observable outcome of a delivery attempt: the HTTP
// status code (0 on transport error) and any Retry-After delay requested on a
// 429/503.
type DeliverResult struct {
	StatusCode int
	RetryAfter time.Duration
}

// Deliver POSTs ev to sub.URL as a signed JSON envelope. deliveryID is the
// stable id sent in X-Forge-Delivery (generate a fresh one for a one-off test
// ping). It returns the attempt result and an error on transport failure or a
// non-2xx response. Exported so an admin "test ping" can reuse it.
func (e *Engine) Deliver(ctx context.Context, sub Subscription, ev Event, deliveryID string) (DeliverResult, error) {
	body, err := json.Marshal(deliveryPayload{SchemaVersion: SchemaVersion, ID: deliveryID, Event: ev})
	if err != nil {
		return DeliverResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return DeliverResult{}, err
	}
	ts := time.Now().UTC().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, ev.Type)
	req.Header.Set(DeliveryHeader, deliveryID)
	req.Header.Set(TimestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(SignatureHeader, Sign(sub.Secret, ts, body))

	resp, err := e.client.Do(req)
	if err != nil {
		return DeliverResult{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	res := DeliverResult{StatusCode: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			res.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return res, fmt.Errorf("webhook: non-2xx from %s: %d", sub.URL, resp.StatusCode)
	}
	return res, nil
}

// parseRetryAfter parses an HTTP Retry-After header value, which is either
// delta-seconds or an HTTP-date. Returns 0 for an absent, malformed, or
// past-dated value.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
