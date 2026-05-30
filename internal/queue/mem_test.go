package queue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestMem_EnqueueWork(t *testing.T) {
	q := NewMem(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen atomic.Int32
	go q.Work(ctx, func(_ context.Context, j Job) error {
		seen.Add(1)
		return nil
	})

	for i := 0; i < 5; i++ {
		if err := q.Enqueue(ctx, "test.job", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	q.Drain()
	if seen.Load() != 5 {
		t.Errorf("expected 5 jobs processed, got %d", seen.Load())
	}
}

func TestMem_DrainWaitsForInFlight(t *testing.T) {
	q := NewMem(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed atomic.Int32
	go q.Work(ctx, func(_ context.Context, _ Job) error {
		time.Sleep(10 * time.Millisecond)
		processed.Add(1)
		return nil
	})

	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, "slow", nil) //nolint:errcheck
	}
	q.Drain()
	if processed.Load() != 3 {
		t.Errorf("expected 3 processed after Drain, got %d", processed.Load())
	}
}

func TestMem_CancelledEnqueue(t *testing.T) {
	q := NewMem(0) // zero buffer — blocks immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := q.Enqueue(ctx, "x", nil)
	if err == nil {
		t.Error("expected error on cancelled enqueue")
	}
}

func TestMem_JobPayloadRoundTrip(t *testing.T) {
	q := NewMem(5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type payload struct{ X int }
	received := make(chan Job, 1)
	go q.Work(ctx, func(_ context.Context, j Job) error {
		received <- j
		return nil
	})

	q.Enqueue(ctx, "typed.job", payload{X: 42}) //nolint:errcheck
	q.Drain()

	j := <-received
	if j.Type != "typed.job" {
		t.Errorf("type: got %q", j.Type)
	}
	var got payload
	if err := j.UnmarshalPayload(&got); err != nil {
		t.Fatal(err)
	}
	if got.X != 42 {
		t.Errorf("payload X: got %d", got.X)
	}
}
