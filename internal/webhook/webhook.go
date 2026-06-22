// Package webhook delivers events (e.g. artifact publish) to configured HTTP
// endpoints, HMAC-signed, durably via the shared work queue.
//
// Subscriptions are persisted in the meta.Store; on a matching event the Engine
// enqueues one delivery job per subscription, and a worker handler (registered
// on the single async worker) POSTs the signed payload with bounded retry.
package webhook

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"forge/internal/meta"
)

// subscriptionsNS is the meta.Store namespace holding subscriptions (key=ID).
const subscriptionsNS = "webhooks"

// Event types forge emits. Subscriptions filter on these (empty filter = all).
const (
	// EventArtifactPublished — a successful write (publish) to a repository.
	EventArtifactPublished = "artifact.published"
	// EventArtifactDeleted — an explicit component+version deletion (admin/API).
	// Deletions made by automated cleanup are summarised by EventCleanupCompleted
	// instead, so a retention run doesn't emit one event per removed version.
	EventArtifactDeleted = "artifact.deleted"
	// EventCleanupCompleted — a cleanup run finished having removed at least one
	// artifact. Data carries policy, deleted, freedBytes, trigger ("scheduled",
	// "on-publish", or "manual").
	EventCleanupCompleted = "cleanup.completed"
	// EventArtifactCached — a proxy repository filled its cache from upstream
	// (a cache miss that fetched and stored the artifact). Fires once per herd
	// (the singleflight leader), not for fresh hits or revalidations.
	EventArtifactCached = "artifact.cached"
)

// AllEventTypes lists every emittable event type, for the admin UI.
var AllEventTypes = []string{
	EventArtifactPublished,
	EventArtifactDeleted,
	EventArtifactCached,
	EventCleanupCompleted,
}

// Subscription is one registered endpoint and its delivery filters.
type Subscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret"`           // HMAC-SHA256 key
	Events    []string  `json:"events,omitempty"` // subscribed event types; empty = all
	Repo      string    `json:"repo,omitempty"`   // repo name filter; "" or "*" = all
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// Matches reports whether this subscription should receive ev. A disabled
// subscription never matches.
func (s Subscription) Matches(ev Event) bool {
	if !s.Enabled {
		return false
	}
	if s.Repo != "" && s.Repo != "*" && s.Repo != ev.Repo {
		return false
	}
	if len(s.Events) == 0 {
		return true
	}
	for _, t := range s.Events {
		if t == ev.Type {
			return true
		}
	}
	return false
}

// Event is the payload delivered to subscribers.
type Event struct {
	Type      string         `json:"type"`
	Repo      string         `json:"repo"`
	Format    string         `json:"format,omitempty"`
	Path      string         `json:"path,omitempty"` // sub-path / component within the repo
	Actor     string         `json:"actor,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	Data      map[string]any `json:"data,omitempty"` // event-specific extras (e.g. cleanup counts)
}

// Store is the subscription CRUD layer over meta.Store. It mirrors
// cleanup.PolicyManager: one JSON document per record under subscriptionsNS.
type Store struct{ meta meta.Store }

// NewStore returns a subscription store backed by m.
func NewStore(m meta.Store) *Store { return &Store{meta: m} }

// List returns all subscriptions, sorted by name then ID for stable output.
func (st *Store) List() ([]Subscription, error) {
	ids, err := st.meta.List(subscriptionsNS)
	if err != nil {
		return nil, err
	}
	subs := make([]Subscription, 0, len(ids))
	for _, id := range ids {
		var sub Subscription
		if ok, _ := st.meta.GetJSON(subscriptionsNS, id, &sub); ok {
			subs = append(subs, sub)
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].Name != subs[j].Name {
			return subs[i].Name < subs[j].Name
		}
		return subs[i].ID < subs[j].ID
	})
	return subs, nil
}

// Get returns the subscription with id, or ok=false if absent.
func (st *Store) Get(id string) (Subscription, bool, error) {
	var sub Subscription
	ok, err := st.meta.GetJSON(subscriptionsNS, id, &sub)
	return sub, ok, err
}

// Create assigns an ID + CreatedAt and persists sub.
func (st *Store) Create(sub Subscription) (Subscription, error) {
	if sub.URL == "" {
		return Subscription{}, fmt.Errorf("webhook: URL is required")
	}
	if sub.ID == "" {
		sub.ID = NewID()
	}
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now().UTC()
	}
	return sub, st.meta.PutJSON(subscriptionsNS, sub.ID, sub)
}

// Delete removes the subscription with id (no error if absent).
func (st *Store) Delete(id string) error {
	return st.meta.Delete(subscriptionsNS, id)
}

// NewID returns a random 16-hex-char identifier (stdlib only). Used for
// subscription IDs and per-delivery IDs.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
