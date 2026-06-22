package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// PG is a Postgres-backed Queue.
//
// Jobs are stored in the `jobs` table (created by migration 002). Dequeuing
// uses DELETE … RETURNING with a sub-select that applies FOR UPDATE SKIP
// LOCKED, so multiple app nodes can safely consume the same queue without
// double-processing.
//
// Failed jobs (fn returned non-nil) are re-inserted so they will be retried.
// There is no dead-letter or back-off yet — that belongs in a later phase.
type PG struct {
	db  *sql.DB
	seq atomic.Int64
}

// NewPG returns a PG queue backed by db.
// The `jobs` table must already exist (created by migrate.Up via meta.NewPG).
func NewPG(db *sql.DB) *PG { return &PG{db: db} }

func (p *PG) Enqueue(ctx context.Context, typ string, payload any) error {
	return p.EnqueueAfter(ctx, typ, payload, 0)
}

func (p *PG) EnqueueAfter(ctx context.Context, typ string, payload any, delay time.Duration) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("queue.PG: marshal payload: %w", err)
	}
	visibleAfter := time.Now()
	if delay > 0 {
		visibleAfter = visibleAfter.Add(delay)
	}
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO jobs (type, payload, visible_after) VALUES ($1, $2, $3)`,
		typ, b, visibleAfter.UTC())
	if err != nil {
		return fmt.Errorf("queue.PG: enqueue: %w", err)
	}
	return nil
}

// Work polls the jobs table in a tight loop, processing one job at a time.
// It backs off 200 ms when the queue is empty to avoid hammering Postgres.
func (p *PG) Work(ctx context.Context, fn func(context.Context, Job) error) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j, err := p.dequeue(ctx)
		if err != nil {
			// Transient DB error; wait before retrying.
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if j == nil {
			// Queue empty; back off before polling again.
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		if ferr := fn(ctx, *j); ferr != nil {
			// Re-insert failed job for retry.
			p.Enqueue(ctx, j.Type, j.Payload) //nolint:errcheck
		}
	}
}

// dequeue atomically claims and removes one job from the head of the queue.
// Returns (nil, nil) when the queue is empty.
func (p *PG) dequeue(ctx context.Context) (*Job, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var j Job
	var rawPayload []byte
	err = tx.QueryRowContext(ctx, `
		DELETE FROM jobs
		WHERE id = (
			SELECT id FROM jobs
			WHERE visible_after <= now()
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, type, payload
	`).Scan(&j.ID, &j.Type, &rawPayload)

	if err == sql.ErrNoRows {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	j.Payload = rawPayload
	return &j, tx.Commit()
}
