package obs

import (
	"context"
	"sync"
	"time"
)

// AuditEntry is a single recorded event — a write request or an auth failure.
type AuditEntry struct {
	Timestamp time.Time
	Actor     string // token description or "anonymous"
	Method    string
	Path      string
	Status    int
	// Detail is an optional human-readable note explaining the event beyond its
	// method/path/status (e.g. a vulnerability-policy decision). Empty for the
	// generic write/auth-failure events. Stored durably so the Activity view can
	// distinguish, say, a warned download from an ordinary one.
	Detail string
}

// AuditRecord is an AuditEntry plus its durable row ID, used as the tiebreaker
// in keyset pagination cursors (timestamps can collide).
type AuditRecord struct {
	AuditEntry
	ID int64
}

// AuditFilter narrows an audit history query. Zero-valued fields mean "no
// filter"; a zero Cursor means "from the newest entry".
type AuditFilter struct {
	Actor    string      // exact actor match when non-empty
	PathLike string      // case-insensitive substring of Path when non-empty
	Cursor   AuditCursor // return only entries strictly older than this
	Limit    int         // max rows; clamped by the implementation
}

// AuditCursor is a keyset-pagination position. IsZero reports the first page.
type AuditCursor struct {
	Timestamp time.Time
	ID        int64
}

// IsZero reports whether the cursor is unset (first page).
func (c AuditCursor) IsZero() bool { return c.ID == 0 && c.Timestamp.IsZero() }

// AuditQuerier is an optional capability for sinks that can serve filtered,
// paginated history beyond the recent window. Only the durable (Postgres) sink
// implements it; callers detect support with a type assertion and fall back to
// Recent otherwise. Implementations must use keyset (not OFFSET) pagination.
type AuditQuerier interface {
	// Query returns up to filter.Limit records, newest first, matching the
	// filter and strictly older than filter.Cursor. The caller forms the next
	// cursor from the last record's (Timestamp, ID).
	Query(ctx context.Context, filter AuditFilter) ([]AuditRecord, error)
}

// AuditSink records audit events and returns the most recent ones. The eval-mode
// implementation is the in-memory *AuditLog ring buffer; the production target is
// a Postgres-backed sink so the Activity view stays coherent across replicas and
// survives restarts. Implementations must be safe for concurrent use.
type AuditSink interface {
	// Append records one event. It must not block the request path.
	Append(e AuditEntry)
	// Recent returns at most n entries, newest first.
	Recent(n int) []AuditEntry
}

// compile-time assertion that the ring buffer satisfies the sink interface.
var _ AuditSink = (*AuditLog)(nil)

// AuditLog is a fixed-capacity thread-safe ring buffer of AuditEntry records.
type AuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	pos     int  // next write index
	full    bool // ring has wrapped at least once
	cap     int
}

// NewAuditLog creates an AuditLog with the given capacity.
func NewAuditLog(capacity int) *AuditLog {
	if capacity <= 0 {
		capacity = 500
	}
	return &AuditLog{entries: make([]AuditEntry, capacity), cap: capacity}
}

// Append adds e to the ring, overwriting the oldest entry when full.
func (al *AuditLog) Append(e AuditEntry) {
	al.mu.Lock()
	al.entries[al.pos] = e
	al.pos = (al.pos + 1) % al.cap
	if al.pos == 0 {
		al.full = true
	}
	al.mu.Unlock()
}

// Recent returns at most n entries, newest first.
func (al *AuditLog) Recent(n int) []AuditEntry {
	al.mu.Lock()
	defer al.mu.Unlock()
	size := al.pos
	if al.full {
		size = al.cap
	}
	if n > size {
		n = size
	}
	out := make([]AuditEntry, n)
	for i := range out {
		idx := (al.pos - 1 - i + al.cap) % al.cap
		out[i] = al.entries[idx]
	}
	return out
}
