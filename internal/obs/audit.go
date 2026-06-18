package obs

import (
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
}

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
