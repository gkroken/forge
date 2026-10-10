//go:build integration

package testutil_test

import (
	"net/http"
	"strings"
	"testing"

	"forge/internal/testutil"
)

func TestStartPostgres(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	if !strings.Contains(dsn, "forge") {
		t.Fatalf("unexpected DSN: %s", dsn)
	}
	t.Logf("postgres DSN: %s", dsn)
}

func TestStartS3(t *testing.T) {
	cfg := testutil.StartS3(t)
	resp, err := http.Get("http://" + cfg.Endpoint + "/healthz")
	if err != nil {
		t.Fatalf("s3 health check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("s3 health: got %d want 200", resp.StatusCode)
	}
	t.Logf("s3 endpoint: %s", cfg.Endpoint)
}
