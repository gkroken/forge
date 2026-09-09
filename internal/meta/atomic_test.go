package meta_test

import (
	"fmt"
	"sync"
	"testing"

	"forge/internal/meta"
)

// A record must never be observed half-written. PutJSON used os.WriteFile,
// which truncates before writing, so a concurrent reader could unmarshal an
// empty or partial document — and every reader here unmarshals what it finds.
//
// The visible symptom was authentication: Verify stamps LastUsed on every
// authenticated request, so the token record is rewritten constantly, and 35 of
// 40 concurrent requests carrying a VALID token were rejected with 401.
func TestPutJSON_ReadersNeverSeeAPartialDocument(t *testing.T) {
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
		Pad   string `json:"pad"`
	}
	// Large enough that a truncating write leaves a wide window open.
	pad := ""
	for i := 0; i < 400; i++ {
		pad += "0123456789"
	}
	if err := m.PutJSON("ns", "k", doc{Name: "seed", Value: 0, Pad: pad}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var badReads, missingReads int64
	var mu sync.Mutex

	// Both loops are bounded: an earlier version signalled the writers to stop
	// immediately after starting them, so they barely ran and the test passed
	// even against the truncating write it exists to catch.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				_ = m.PutJSON("ns", "k", doc{Name: fmt.Sprintf("w%d-%d", w, i), Value: i, Pad: pad})
			}
		}(w)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 600; i++ {
				var got doc
				ok, err := m.GetJSON("ns", "k", &got)
				mu.Lock()
				switch {
				case err != nil || (ok && got.Pad != pad):
					badReads++ // torn or unparseable
				case !ok:
					missingReads++ // vanished mid-write
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if badReads > 0 || missingReads > 0 {
		t.Errorf("readers saw %d torn and %d missing documents; a record being "+
			"rewritten must stay readable", badReads, missingReads)
	}
}
