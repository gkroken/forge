package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"forge/internal/format"
	"forge/internal/integrity"
	"forge/internal/queue"
	"forge/internal/repo"
)

// integrityJobType is the async job that verifies one repository's storage.
// It runs on the shared worker (see WithQueue) — the single worker also
// serializes verify runs, so a full-mode pass over a big repo never competes
// with a second one.
const integrityJobType = "integrity.verify"

type integrityPayload struct {
	Repo string `json:"repo"`
	Mode string `json:"mode"`
}

// integrityStaleAfter is how long a queued/running report blocks re-enqueue
// before it is presumed abandoned (worker died mid-run) and a new run is
// allowed.
const integrityStaleAfter = 2 * time.Hour

// proxyVerifyNote is attached to every proxy-repo report: findings there are
// hygiene, not data loss.
const proxyVerifyNote = "proxy caches are self-healing — missing cached content is re-fetched from upstream on demand"

// handleIntegrityJob is the worker handler for integrity.verify jobs. Always
// returns nil: the outcome (including failure) is recorded in the report
// itself, and a queue retry would just re-read the same store.
func (s *Server) handleIntegrityJob(ctx context.Context, j queue.Job) error {
	var p integrityPayload
	if err := j.UnmarshalPayload(&p); err != nil {
		slog.Warn("integrity: bad verify payload", "err", err)
		return nil
	}
	mode, ok := integrity.ParseMode(p.Mode)
	if !ok {
		mode = integrity.ModeFull
	}
	s.verifyRepoIntegrity(p.Repo, mode)
	return nil
}

// verifyRepoIntegrity runs one read-only verify pass and persists the report
// (replacing any previous one). It never touches repository content.
func (s *Server) verifyRepoIntegrity(repoName string, mode integrity.Mode) {
	store := integrity.NewStore(s.Meta)
	rp, ok := s.Repos.Get(repoName)
	if !ok {
		slog.Warn("integrity: repository not found", "repo", repoName)
		return
	}

	// Preserve QueuedAt from the enqueue-time record, then mark running.
	prev, _, _ := store.Get(repoName)
	queued := prev.QueuedAt
	started := time.Now().UTC()
	running := integrity.Report{
		Repo: repoName, Status: integrity.StatusRunning, Mode: mode,
		QueuedAt: queued, StartedAt: started,
		Findings: []integrity.Finding{}, Counts: map[string]int{},
	}
	if err := store.Put(running); err != nil {
		slog.Warn("integrity: store running marker failed", "repo", repoName, "err", err)
	}

	var res integrity.Result
	var note string
	var runErr error
	switch {
	case rp.Kind == repo.Group:
		note = "group repositories own no storage — verify the member repositories individually"
	default:
		h, ok := s.Handlers.For(rp.Format)
		if !ok {
			note = "no handler for format " + rp.Format
			break
		}
		checker, ok := h.(format.IntegrityChecker)
		if !ok {
			note = "format " + rp.Format + " has no integrity checker; nothing was verified"
			break
		}
		c := &format.Context{
			Repo: rp, Blob: s.Blob, Meta: s.Meta, HTTP: s.client,
			Repos: s.Repos, Metrics: s.Metrics,
		}
		res, runErr = checker.VerifyIntegrity(c, mode)
		if rp.Kind == repo.Proxy {
			note = proxyVerifyNote
		}
	}

	if runErr != nil {
		rep := integrity.Report{
			Repo: repoName, Status: integrity.StatusFailed, Mode: mode,
			QueuedAt: queued, StartedAt: started, FinishedAt: time.Now().UTC(),
			Findings: []integrity.Finding{}, Counts: map[string]int{},
			Error: runErr.Error(),
		}
		rep.DurationMs = rep.FinishedAt.Sub(started).Milliseconds()
		if err := store.Put(rep); err != nil {
			slog.Warn("integrity: store failed report failed", "repo", repoName, "err", err)
		}
		slog.Warn("integrity: verify failed", "repo", repoName, "err", runErr)
		return
	}

	rep := integrity.BuildReport(repoName, mode, queued, started, res, note)
	if err := store.Put(rep); err != nil {
		slog.Warn("integrity: store report failed", "repo", repoName, "err", err)
		return
	}
	slog.Info("integrity: verify complete", "repo", repoName, "mode", mode,
		"findings", rep.TotalFindings, "blobs", rep.BlobsChecked, "bytes", rep.BytesRead,
		"duration_ms", rep.DurationMs)
}

// handleVerify serves /api/v1/repos/{name}/verify (repo-admin gated by the
// caller):
//
//	GET  → the latest report, or {"repo":…,"status":"never"} if none exists.
//	POST → enqueue an async verify; ?mode=quick|full (default full). 202 on
//	       enqueue; if a run is already queued/running (and not stale) the
//	       response is 202 with the current status and nothing is re-enqueued.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request, repoName string) {
	store := integrity.NewStore(s.Meta)
	if _, ok := s.Repos.Get(repoName); !ok {
		jsonError(w, "repository not found: "+repoName, http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		rep, ok, err := store.Get(repoName)
		if err != nil {
			jsonError(w, "read report: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			writeJSON(w, map[string]string{"repo": repoName, "status": "never"})
			return
		}
		writeJSON(w, rep)

	case http.MethodPost:
		if s.Queue == nil {
			jsonError(w, "async worker not configured", http.StatusServiceUnavailable)
			return
		}
		mode, ok := integrity.ParseMode(r.URL.Query().Get("mode"))
		if !ok {
			jsonError(w, "mode must be quick or full", http.StatusBadRequest)
			return
		}
		// Dedupe: one verify in flight per repo.
		if prev, found, _ := store.Get(repoName); found &&
			(prev.Status == integrity.StatusQueued || prev.Status == integrity.StatusRunning) {
			ref := prev.QueuedAt
			if prev.StartedAt.After(ref) {
				ref = prev.StartedAt
			}
			if time.Since(ref) < integrityStaleAfter {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				json.NewEncoder(w).Encode(map[string]string{"status": prev.Status, "repo": repoName}) //nolint:errcheck
				return
			}
			// Stale queued/running marker: presume the worker died; re-enqueue.
		}
		if err := store.Put(integrity.Report{
			Repo: repoName, Status: integrity.StatusQueued, Mode: mode,
			QueuedAt: time.Now().UTC(),
			Findings: []integrity.Finding{}, Counts: map[string]int{},
		}); err != nil {
			jsonError(w, "store queued marker: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.Queue.Enqueue(r.Context(), integrityJobType, integrityPayload{Repo: repoName, Mode: string(mode)}); err != nil {
			jsonError(w, "enqueue failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "verify enqueued", "repo": repoName, "mode": string(mode)}) //nolint:errcheck

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// integrityRollupRow summarizes one repo for the fleet-wide rollup.
type integrityRollupRow struct {
	Repo   string            `json:"repo"`
	Format string            `json:"format"`
	Kind   string            `json:"kind"`
	Report *integrity.Report `json:"report,omitempty"` // nil = never verified
}

// handleIntegrityRollup serves GET /api/v1/integrity — the latest report per
// repository (global admin).
func (s *Server) handleIntegrityRollup(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := integrity.NewStore(s.Meta)
	rows := []integrityRollupRow{}
	for _, rp := range s.Repos.All() {
		row := integrityRollupRow{Repo: rp.Name, Format: rp.Format, Kind: string(rp.Kind)}
		if rep, ok, err := store.Get(rp.Name); err == nil && ok {
			r := rep
			row.Report = &r
		}
		rows = append(rows, row)
	}
	writeJSON(w, rows)
}
