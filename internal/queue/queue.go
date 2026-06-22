// Package queue defines a simple work queue for async index regeneration.
//
// Callers Enqueue a typed job with a JSON payload; one or more workers call
// Work to drain the queue by invoking a handler function per job.
// The handler must be idempotent: receiving the same job twice must produce
// the same final state (queues may deliver jobs more than once).
//
// Two implementations are provided:
//
//   - Mem   — in-process, channel-backed; suitable for eval / single-node.
//             State is lost on process restart.
//   - PG    — Postgres-backed using SELECT … FOR UPDATE SKIP LOCKED;
//             safe for multiple concurrent app nodes sharing one database.
//
// Lifecycle: a Queue must be long-lived (one per server). Create it during
// server start-up, call Work in a background goroutine, cancel the context
// when the server shuts down.
package queue

import (
	"context"
	"encoding/json"
	"time"
)

// Job is one unit of dequeued work.
type Job struct {
	ID      string
	Type    string
	Payload json.RawMessage
}

// UnmarshalPayload JSON-decodes the job's payload into v.
func (j Job) UnmarshalPayload(v any) error { return json.Unmarshal(j.Payload, v) }

// DepthReader is an optional extension to Queue that exposes pending job count.
// Type-assert the Queue to this interface before calling.
type DepthReader interface {
	Depth() int
}

// TaskInfo describes one job that has been processed (or is currently running).
type TaskInfo struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"` // "running" | "done" | "failed"
	StartedAt time.Time `json:"started_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

// Queue is the core interface: enqueue work, drain it via Work.
// Implementations must be safe for concurrent use.
type Queue interface {
	// Enqueue adds a job with the given type and a JSON-serialisable payload.
	// It returns as soon as the job is accepted; processing is asynchronous.
	// Equivalent to EnqueueAfter with a zero delay.
	Enqueue(ctx context.Context, typ string, payload any) error

	// EnqueueAfter adds a job that only becomes eligible for processing once
	// delay has elapsed (delay <= 0 means immediately). This backs delayed
	// retries such as webhook-delivery backoff: the durable PG impl persists the
	// visibility time so a delayed retry survives restarts; the in-memory impl
	// schedules the enqueue.
	EnqueueAfter(ctx context.Context, typ string, payload any, delay time.Duration) error

	// Work blocks until ctx is cancelled, calling fn for each job.
	// If fn returns a non-nil error the job may be retried (impl-specific).
	// fn must be idempotent.
	Work(ctx context.Context, fn func(context.Context, Job) error) error
}
