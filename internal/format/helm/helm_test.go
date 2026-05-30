package helm

import (
	"testing"
	"time"

	"forge/internal/golden"
)

var fixedNow = time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)

func TestBuildIndex_Golden(t *testing.T) {
	recs := []chartRecord{
		{
			Name: "webapp", Version: "0.4.1", AppVersion: "2.0",
			Description: "A demo web application chart",
			Digest:      "aaabbbccc", Created: "2024-01-15T10:00:00Z",
			Filename: "webapp-0.4.1.tgz",
		},
		{
			Name: "webapp", Version: "0.3.0",
			Description: "A demo web application chart",
			Digest:      "dddeeefff", Created: "2024-01-14T09:00:00Z",
			Filename: "webapp-0.3.0.tgz",
		},
		{
			Name: "redis", Version: "1.0.0",
			Description: "Redis chart",
			Digest:      "111222333", Created: "2024-01-13T08:00:00Z",
			Filename: "redis-1.0.0.tgz",
		},
	}
	got := []byte(buildIndex(recs, fixedNow))
	golden.Assert(t, got, "index_two_charts.yaml")
}

func TestBuildIndex_Empty(t *testing.T) {
	got := buildIndex(nil, fixedNow)
	want := "apiVersion: v1\nentries:\ngenerated: 2024-01-15T12:00:00Z\n"
	if got != want {
		t.Fatalf("empty index mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}
