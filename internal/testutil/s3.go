package testutil

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// S3Image is the S3-compatible server the integration tests run against:
// SeaweedFS, pinned by digest so an upstream retag or deletion cannot change
// or break the test backend overnight — which is what happened when MinIO
// removed its community images from Docker Hub (2026-09-11).
const S3Image = "chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"

// S3Config holds the connection parameters for an ephemeral S3 server.
type S3Config struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Bucket    string
}

// StartS3 starts a single-container SeaweedFS (master, volume, filer and S3
// gateway) and returns its config. The container is terminated automatically
// when t finishes.
func StartS3(t *testing.T) S3Config {
	t.Helper()
	ctx := context.Background()
	const access, secret = "forgetest", "forgetest-secret"
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        S3Image,
			Cmd:          []string{"server", "-s3", "-dir=/data"},
			ExposedPorts: []string{"8333/tcp"},
			// SeaweedFS takes S3 credentials from these when no -s3.config is given.
			Env:        map[string]string{"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret},
			WaitingFor: wait.ForHTTP("/healthz").WithPort("8333/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("testutil: start s3 (seaweedfs): %v", err)
	}
	t.Cleanup(func() { c.Terminate(context.Background()) }) //nolint:errcheck

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("testutil: s3 host: %v", err)
	}
	port, err := c.MappedPort(ctx, "8333/tcp")
	if err != nil {
		t.Fatalf("testutil: s3 port: %v", err)
	}
	return S3Config{Endpoint: host + ":" + port.Port(), AccessKey: access, SecretKey: secret, Bucket: "forge"}
}
