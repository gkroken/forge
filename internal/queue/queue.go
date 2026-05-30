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
)

// Job is one unit of dequeued work.
type Job struct {
	ID      string
	Type    string
	Payload json.RawMessage
}

// UnmarshalPayload JSON-decodes the job's payload into v.
func (j Job) UnmarshalPayload(v any) error { return json.Unmarshal(j.Payload, v) }

// Queue is the core interface: enqueue work, drain it via Work.
// Implementations must be safe for concurrent use.
type Queue interface {
	// Enqueue adds a job with the given type and a JSON-serialisable payload.
	// It returns as soon as the job is accepted; processing is asynchronous.
	Enqueue(ctx context.Context, typ string, payload any) error

	// Work blocks until ctx is cancelled, calling fn for each job.
	// If fn returns a non-nil error the job may be retried (impl-specific).
	// fn must be idempotent.
	Work(ctx context.Context, fn func(context.Context, Job) error) error
}
