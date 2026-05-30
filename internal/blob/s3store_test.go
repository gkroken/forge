//go:build integration

package blob_test

import (
	"testing"

	"forge/internal/blob"
	"forge/internal/blob/blobtest"
	"forge/internal/testutil"
)

func TestS3_Contract(t *testing.T) {
	cfg := testutil.StartMinio(t)
	s, err := blob.NewS3(blob.S3Config{
		Endpoint:  cfg.Endpoint,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
		Bucket:    cfg.Bucket,
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	blobtest.RunContract(t, s)
}
