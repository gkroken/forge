package testutil

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// MinioConfig holds the connection parameters for an ephemeral MinIO instance.
type MinioConfig struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Bucket    string
}

// StartMinio starts a MinIO container and returns its config.
// The container is terminated automatically when t finishes.
func StartMinio(t *testing.T) MinioConfig {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "quay.io/minio/minio:latest",
			Cmd:          []string{"server", "/data"},
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     "minioadmin",
				"MINIO_ROOT_PASSWORD": "minioadmin",
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").WithPort("9000/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("testutil: start minio: %v", err)
	}
	t.Cleanup(func() { c.Terminate(context.Background()) }) //nolint:errcheck

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("testutil: minio host: %v", err)
	}
	port, err := c.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("testutil: minio port: %v", err)
	}
	return MinioConfig{
		Endpoint:  host + ":" + port.Port(),
		AccessKey: "minioadmin",
		SecretKey: "minioadmin",
		Bucket:    "forge",
	}
}
