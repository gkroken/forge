package webhook

import (
	"sync"
	"time"

	"forge/internal/meta"
)

// deliveriesNS is the meta.Store namespace holding per-subscription delivery
// history (key = subscription ID, value = capped newest-first slice).
const deliveriesNS = "webhook-deliveries"

// maxHistoryPerSub caps how many recent delivery records are retained per
// subscription. A small ring keeps the meta document bounded; the dead-letter
// (dropped) records age out with the rest, which is acceptable for an operator
// console — alerting should consume the Prometheus counter, not this log.
const maxHistoryPerSub = 50

// Delivery outcome statuses recorded in history and used as the metric label.
const (
	StatusSuccess = "success" // a 2xx response
	StatusFailed  = "failed"  // a failed attempt that will be retried
	StatusDropped = "dropped" // the final failed attempt — the dead-letter
)

// DeliveryRecord is one delivery attempt's outcome, persisted for the operator
// deliveries panel. One record per attempt, so a flapping endpoint shows its
// retry trail and the terminal "dropped" entry.
type DeliveryRecord struct {
	ID        string    `json:"id"`              // delivery id (stable across a delivery's retries)
	SubID     string    `json:"subID"`           // subscription ID
	Event     string    `json:"event"`           // event type
	Repo      string    `json:"repo,omitempty"`  // event repo
	Status    string    `json:"status"`          // success | failed | dropped
	HTTPCode  int       `json:"httpCode"`        // response status (0 on transport error)
	Attempt   int       `json:"attempt"`         // 1-based attempt number
	Error     string    `json:"error,omitempty"` // error detail for failed/dropped
	Timestamp time.Time `json:"timestamp"`
}

// History is the per-subscription delivery log over meta.Store. Records are kept
// newest-first, capped at maxHistoryPerSub. A mutex serialises the
// read-modify-write so concurrent worker deliveries don't lose records.
type History struct {
	meta meta.Store
	cap  int
	mu   sync.Mutex
}

// NewHistory returns a delivery-history store backed by m.
func NewHistory(m meta.Store) *History { return &History{meta: m, cap: maxHistoryPerSub} }

// Append records rec for its subscription, trimming to the most recent cap.
func (h *History) Append(rec DeliveryRecord) {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var recs []DeliveryRecord
	h.meta.GetJSON(deliveriesNS, rec.SubID, &recs) //nolint:errcheck
	recs = append([]DeliveryRecord{rec}, recs...)
	if len(recs) > h.cap {
		recs = recs[:h.cap]
	}
	h.meta.PutJSON(deliveriesNS, rec.SubID, recs) //nolint:errcheck
}

// List returns the recent delivery records for subID, newest first.
func (h *History) List(subID string) ([]DeliveryRecord, error) {
	var recs []DeliveryRecord
	if _, err := h.meta.GetJSON(deliveriesNS, subID, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// Delete drops a subscription's delivery history (used when the sub is removed).
func (h *History) Delete(subID string) error {
	return h.meta.Delete(deliveriesNS, subID)
}
