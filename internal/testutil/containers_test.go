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

func TestStartMinio(t *testing.T) {
	cfg := testutil.StartMinio(t)
	resp, err := http.Get("http://" + cfg.Endpoint + "/minio/health/live")
	if err != nil {
		t.Fatalf("minio health check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("minio health: got %d want 200", resp.StatusCode)
	}
	t.Logf("minio endpoint: %s", cfg.Endpoint)
}
