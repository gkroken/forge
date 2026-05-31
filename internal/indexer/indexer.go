// Package indexer provides idempotent index-regeneration workers.
//
// Format handlers enqueue a job on every write (publish, unpublish, etc.).
// The worker reads the current state of all source records and rebuilds the
// materialized index from scratch, so running the same job N times always
// produces the same result regardless of how many concurrent publishes fired.
//
// Current job types:
//
//	npm.regen   — rebuild npm packument from per-version records
package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/queue"
)

// RegenPayload is the job payload shared by all "regen" job types.
type RegenPayload struct {
	RepoName string `json:"repoName"`
	Pkg      string `json:"pkg"`
}

// Worker processes index-regeneration jobs from a Queue.
// It must be long-lived — create one and call Work in a background goroutine.
type Worker struct {
	meta    meta.Store
	metrics *obs.Metrics
}

// New returns a Worker backed by the given meta store.
func New(m meta.Store) *Worker { return &Worker{meta: m} }

// WithMetrics attaches Prometheus instruments to the Worker so job outcomes
// are recorded against forge_queue_jobs_total{type, result}.
func (w *Worker) WithMetrics(metrics *obs.Metrics) *Worker {
	w.metrics = metrics
	return w
}

// Work drains q until ctx is cancelled, dispatching each job to the
// appropriate regeneration function.
func (w *Worker) Work(ctx context.Context, q queue.Queue) error {
	return q.Work(ctx, func(ctx context.Context, j queue.Job) error {
		err := w.dispatch(j)
		if w.metrics != nil {
			result := "success"
			if err != nil {
				result = "error"
			}
			w.metrics.QueueJobsTotal.WithLabelValues(j.Type, result).Inc()
		}
		return err
	})
}

func (w *Worker) dispatch(j queue.Job) error {
	switch j.Type {
	case "npm.regen":
		return w.regenNPM(j)
	default:
		return nil // unknown job type; discard
	}
}

// regenNPM rebuilds the materialized npm packument for one package from the
// per-version records stored during publish.  It is idempotent: calling it
// multiple times for the same package produces the same packument.
//
// Storage layout (all keys in the meta store):
//
//	{repo}:npm:v  /  {pkg}:{ver}   → individual version object
//	{repo}:npm:dt /  {pkg}         → dist-tags map
//	{repo}:npm    /  {pkg}         → materialized packument  (written here)
func (w *Worker) regenNPM(j queue.Job) error {
	var p RegenPayload
	if err := j.UnmarshalPayload(&p); err != nil {
		return fmt.Errorf("indexer: regenNPM: bad payload: %w", err)
	}

	versNS := p.RepoName + ":npm:v"
	tagsNS := p.RepoName + ":npm:dt"
	packNS := p.RepoName + ":npm"

	// Collect all version objects for this package.
	allKeys, err := w.meta.List(versNS)
	if err != nil {
		return fmt.Errorf("indexer: regenNPM: list versions: %w", err)
	}
	prefix := p.Pkg + ":"
	versions := map[string]json.RawMessage{}
	for _, k := range allKeys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		ver := k[len(prefix):]
		var vobj json.RawMessage
		if ok, _ := w.meta.GetJSON(versNS, k, &vobj); ok {
			versions[ver] = vobj
		}
	}

	// If no per-version records exist, the packument predates this storage
	// model; leave it untouched.
	if len(versions) == 0 {
		return nil
	}

	var distTags map[string]json.RawMessage
	w.meta.GetJSON(tagsNS, p.Pkg, &distTags) //nolint:errcheck
	if distTags == nil {
		distTags = map[string]json.RawMessage{}
	}

	packument := map[string]any{
		"name":      p.Pkg,
		"versions":  versions,
		"dist-tags": distTags,
	}
	if err := w.meta.PutJSON(packNS, p.Pkg, packument); err != nil {
		return fmt.Errorf("indexer: regenNPM: write packument: %w", err)
	}
	return nil
}
