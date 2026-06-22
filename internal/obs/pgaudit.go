package obs

import (
	"context"
	"database/sql"
	"log/slog"
	"sync/atomic"
)

// auditBufferSize bounds the in-flight audit queue. Audit events are only
// emitted for writes and auth failures (not every GET), so this is generous.
const auditBufferSize = 1024

// PGAuditSink is a Postgres-backed AuditSink. Entries are buffered and written
// by a single background goroutine so Append never blocks the request path; the
// goroutine stops when the context passed to NewPGAuditSink is cancelled.
//
// Unlike the in-memory ring buffer, records are durable and shared across all
// replicas, so the Activity view is coherent fleet-wide and survives restarts.
//
// The audit_log table must already exist (created by migrate.Up via meta.NewPG).
type PGAuditSink struct {
	db      *sql.DB
	ch      chan AuditEntry
	dropped atomic.Uint64
}

var _ AuditSink = (*PGAuditSink)(nil)

// NewPGAuditSink returns a sink writing to db and starts its drain goroutine.
func NewPGAuditSink(ctx context.Context, db *sql.DB) *PGAuditSink {
	s := &PGAuditSink{db: db, ch: make(chan AuditEntry, auditBufferSize)}
	go s.run(ctx)
	return s
}

// Append enqueues e for asynchronous insertion. If the buffer is full it drops
// the entry rather than block the caller — we never want auditing to slow or
// stall a request. Drops are counted and logged.
func (s *PGAuditSink) Append(e AuditEntry) {
	select {
	case s.ch <- e:
	default:
		n := s.dropped.Add(1)
		slog.Warn("audit: buffer full, dropping entry", "dropped_total", n, "method", e.Method, "path", e.Path)
	}
}

func (s *PGAuditSink) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.ch:
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO audit_log (ts, actor, method, path, status) VALUES ($1, $2, $3, $4, $5)`,
				e.Timestamp, e.Actor, e.Method, e.Path, e.Status,
			); err != nil {
				slog.Warn("audit: insert failed", "err", err, "method", e.Method, "path", e.Path)
			}
		}
	}
}

// Recent returns at most n entries, newest first.
func (s *PGAuditSink) Recent(n int) []AuditEntry {
	if n <= 0 {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT ts, actor, method, path, status FROM audit_log ORDER BY ts DESC, id DESC LIMIT $1`, n)
	if err != nil {
		slog.Warn("audit: recent query failed", "err", err)
		return nil
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Timestamp, &e.Actor, &e.Method, &e.Path, &e.Status); err != nil {
			slog.Warn("audit: recent scan failed", "err", err)
			return out
		}
		out = append(out, e)
	}
	return out
}
