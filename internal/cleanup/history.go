package cleanup

import (
	"sort"
	"time"

	"forge/internal/meta"
)

const (
	historyNSSuffix = ":cleanup:history"
	maxHistory      = 20
)

// CleanupRun records the outcome of a single cleanup execution.
type CleanupRun struct {
	Timestamp  time.Time `json:"timestamp"`
	PolicyName string    `json:"policyName,omitempty"`
	Deleted    int       `json:"deleted"`
	FreedBytes int64     `json:"freedBytes"`
	DurationMs int64     `json:"durationMs"`
	DryRun     bool      `json:"dryRun,omitempty"`
}

// RecordRun persists a CleanupRun to meta.Store and trims the history to the
// most recent maxHistory entries.
func RecordRun(m meta.Store, repoName string, run CleanupRun) error {
	ns := repoName + historyNSSuffix
	key := run.Timestamp.UTC().Format(time.RFC3339Nano)
	if err := m.PutJSON(ns, key, run); err != nil {
		return err
	}
	return trimHistory(m, ns)
}

// GetHistory returns the cleanup run history for a repository, newest first.
// Returns at most maxHistory entries.
func GetHistory(m meta.Store, repoName string) ([]CleanupRun, error) {
	ns := repoName + historyNSSuffix
	keys, err := m.List(ns)
	if err != nil {
		return nil, err
	}
	var runs []CleanupRun
	for _, k := range keys {
		var run CleanupRun
		if ok, _ := m.GetJSON(ns, k, &run); ok {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Timestamp.After(runs[j].Timestamp)
	})
	return runs, nil
}

func trimHistory(m meta.Store, ns string) error {
	keys, err := m.List(ns)
	if err != nil {
		return err
	}
	if len(keys) <= maxHistory {
		return nil
	}
	// Keys are RFC3339Nano strings; lexicographic sort = chronological order.
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-maxHistory] {
		m.Delete(ns, k) //nolint:errcheck
	}
	return nil
}
