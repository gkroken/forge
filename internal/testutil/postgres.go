// Package testutil provides helpers for spinning up ephemeral infrastructure
// in integration tests. Containers are started with testcontainers-go and
// automatically terminated when the test finishes.
package testutil

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartPostgres starts a Postgres 16 container and returns its DSN.
// The container is terminated automatically when t finishes.
func StartPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("forge"),
		postgres.WithUsername("forge"),
		postgres.WithPassword("forge"),
		// Wait for the log line that confirms postgres is accepting connections,
		// not just that the port is open (the default strategy).
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("testutil: start postgres: %v", err)
	}
	t.Cleanup(func() { c.Terminate(context.Background()) }) //nolint:errcheck
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testutil: postgres connection string: %v", err)
	}
	return dsn
}
