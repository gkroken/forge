package queue

import (
	"context"
	"sync"
	"time"
)

// TaskRing records the last N completed jobs so the system tasks API can
// show recent activity. It also tracks the currently-running job.
// Safe for concurrent use.
type TaskRing struct {
	mu      sync.Mutex
	cap     int
	running *TaskInfo   // nil when no job is in progress
	done    []TaskInfo  // ring slice, newest first, capped at cap
}

func NewTaskRing(cap int) *TaskRing {
	return &TaskRing{cap: cap}
}

// Wrap returns a Work-compatible handler that records lifecycle into the ring.
// Pass the wrapped function to queue.Queue.Work instead of the raw handler.
func (tr *TaskRing) Wrap(fn func(context.Context, Job) error) func(context.Context, Job) error {
	return func(ctx context.Context, j Job) error {
		info := &TaskInfo{Name: j.Type, Status: "running", StartedAt: time.Now()}
		tr.mu.Lock()
		tr.running = info
		tr.mu.Unlock()

		err := fn(ctx, j)

		tr.mu.Lock()
		tr.running = nil
		entry := TaskInfo{Name: j.Type, StartedAt: info.StartedAt, DoneAt: time.Now()}
		if err != nil {
			entry.Status = "failed"
		} else {
			entry.Status = "done"
		}
		tr.done = append([]TaskInfo{entry}, tr.done...)
		if len(tr.done) > tr.cap {
			tr.done = tr.done[:tr.cap]
		}
		tr.mu.Unlock()
		return err
	}
}

// Recent returns up to n tasks: the running job first (if any), then the most
// recently completed jobs, newest first.
func (tr *TaskRing) Recent(n int) []TaskInfo {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []TaskInfo
	if tr.running != nil {
		out = append(out, *tr.running)
	}
	for _, t := range tr.done {
		if len(out) >= n {
			break
		}
		out = append(out, t)
	}
	return out
}
