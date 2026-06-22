package obs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// auditBufferSize bounds the in-flight audit queue. Audit events are only
	// emitted for writes and auth failures (not every GET), so this is generous.
	auditBufferSize = 1024
	// auditPruneInterval is how often expired entries are pruned.
	auditPruneInterval = time.Hour
	// auditPruneBatch caps each DELETE so pruning never runs as one giant
	// table-locking, bloat-inducing transaction.
	auditPruneBatch = 10000
)

// PGAuditSink is a Postgres-backed AuditSink. Entries are buffered and written
// by a single background goroutine so Append never blocks the request path; the
// goroutine stops when the context passed to NewPGAuditSink is cancelled.
//
// Unlike the in-memory ring buffer, records are durable and shared across all
// replicas, so the Activity view is coherent fleet-wide and survives restarts.
// A second goroutine prunes entries older than the retention window.
//
// The audit_log table must already exist (created by migrate.Up via meta.NewPG).
type PGAuditSink struct {
	db        *sql.DB
	ch        chan AuditEntry
	dropped   atomic.Uint64
	retention time.Duration // entries older than this are pruned; <=0 disables
}

var (
	_ AuditSink    = (*PGAuditSink)(nil)
	_ AuditQuerier = (*PGAuditSink)(nil)
)

// NewPGAuditSink returns a sink writing to db and starts its drain goroutine.
// When retention > 0 a pruner goroutine deletes entries older than retention.
func NewPGAuditSink(ctx context.Context, db *sql.DB, retention time.Duration) *PGAuditSink {
	s := &PGAuditSink{db: db, ch: make(chan AuditEntry, auditBufferSize), retention: retention}
	go s.run(ctx)
	if retention > 0 {
		go s.pruneLoop(ctx)
	}
	return s
}

// pruneLoop prunes once at startup (so a restart doesn't wait a full interval)
// then on every tick until ctx is cancelled.
func (s *PGAuditSink) pruneLoop(ctx context.Context) {
	prune := func() {
		if n, err := s.Prune(ctx); err != nil {
			slog.Warn("audit: prune failed", "err", err)
		} else if n > 0 {
			slog.Info("audit: pruned expired entries", "deleted", n, "retention", s.retention)
		}
	}
	prune()
	t := time.NewTicker(auditPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// Prune deletes entries older than the retention window in bounded batches and
// returns the total rows removed. Batching keeps each statement short so it
// never holds a long lock or bloats the table. It runs automatically on a timer
// but is safe to call manually (e.g. for an operator-triggered cleanup).
func (s *PGAuditSink) Prune(ctx context.Context) (int64, error) {
	if s.retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-s.retention)
	var total int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM audit_log WHERE id IN (
			     SELECT id FROM audit_log WHERE ts < $1 ORDER BY id LIMIT $2
			 )`, cutoff, auditPruneBatch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < auditPruneBatch {
			return total, nil
		}
		// Yield between batches so a large backlog never monopolises the pool.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
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

// Query returns filtered history newest-first using keyset pagination on
// (ts, id) — the audit_log_ts_idx index serves the ordering directly, and we
// avoid OFFSET so deep pages stay O(limit) rather than O(offset). All filter
// values are bound as parameters.
func (s *PGAuditSink) Query(ctx context.Context, f AuditFilter) ([]AuditRecord, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var (
		conds []string
		args  []any
	)
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if !f.Cursor.IsZero() {
		// Row-value comparison gives a correct, index-friendly keyset seek.
		args = append(args, f.Cursor.Timestamp, f.Cursor.ID)
		conds = append(conds, fmt.Sprintf("(ts, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}
	if f.PathLike != "" {
		add("path ILIKE '%%' || $%d || '%%'", f.PathLike)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	q := fmt.Sprintf(
		`SELECT ts, actor, method, path, status, id FROM audit_log %s ORDER BY ts DESC, id DESC LIMIT $%d`,
		where, len(args))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		if err := rows.Scan(&r.Timestamp, &r.Actor, &r.Method, &r.Path, &r.Status, &r.ID); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
