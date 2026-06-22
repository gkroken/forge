package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Mem is an in-process, channel-backed Queue. It is safe for concurrent use
// but loses state on process restart. Suitable for eval mode and tests.
type Mem struct {
	ch  chan Job
	wg  sync.WaitGroup // counts jobs that have been accepted but not yet processed
	seq atomic.Int64
}

// NewMem returns a Mem queue with the given channel buffer capacity.
// A capacity of ≥ the expected burst of concurrent publishes avoids blocking.
func NewMem(cap int) *Mem {
	return &Mem{ch: make(chan Job, cap)}
}

// Enqueue buffers a job. If the buffer is full it blocks until space is
// available or ctx is cancelled.
func (m *Mem) Enqueue(ctx context.Context, typ string, payload any) error {
	return m.EnqueueAfter(ctx, typ, payload, 0)
}

// EnqueueAfter buffers a job, delaying its availability by delay. With delay<=0
// it behaves like Enqueue (blocking on a full buffer until space or ctx).
// With delay>0 it returns immediately and schedules the buffered send via a
// timer; the job is counted toward Drain at call time so test synchronisation
// still accounts for in-flight delayed retries. Delayed sends are best-effort
// across shutdown (the in-memory queue is not durable).
func (m *Mem) EnqueueAfter(ctx context.Context, typ string, payload any, delay time.Duration) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("queue.Mem: marshal payload: %w", err)
	}
	j := Job{
		ID:      fmt.Sprintf("mem-%d", m.seq.Add(1)),
		Type:    typ,
		Payload: b,
	}
	m.wg.Add(1)
	if delay <= 0 {
		select {
		case m.ch <- j:
			return nil
		case <-ctx.Done():
			m.wg.Done() // undo the Add; job was never queued
			return ctx.Err()
		}
	}
	time.AfterFunc(delay, func() {
		select {
		case m.ch <- j:
		case <-ctx.Done():
			m.wg.Done() // shutting down before the delayed send landed
		}
	})
	return nil
}

// Work processes jobs until ctx is cancelled. fn errors are logged but do
// not stop processing (jobs are not re-queued in the in-memory impl).
func (m *Mem) Work(ctx context.Context, fn func(context.Context, Job) error) error {
	for {
		select {
		case j := <-m.ch:
			fn(ctx, j) //nolint:errcheck — mem queue drops failed jobs
			m.wg.Done()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Drain blocks until all jobs that have been Enqueued are processed.
// Used in tests to synchronise after a burst of publishes.
func (m *Mem) Drain() { m.wg.Wait() }

// Depth returns the number of jobs currently waiting in the channel buffer.
func (m *Mem) Depth() int { return len(m.ch) }
