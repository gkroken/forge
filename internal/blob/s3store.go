package blob

import (
	"bytes"
	"context"
	"crypto/md5"  // #nosec G501 -- MD5/SHA1 required by Maven/npm protocol specs
	"crypto/sha1" // #nosec G505
	"crypto/sha256"
	"encoding/hex"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config holds connection parameters for an S3-compatible object store.
type S3Config struct {
	Endpoint  string // host:port or host
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// S3 implements Store backed by an S3-compatible object store (AWS S3, MinIO, GCS, ...).
// The bucket is created on construction if it does not already exist.
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3 creates a MinIO/S3 client, ensures the bucket exists, and returns a
// ready Store.
func NewS3(cfg S3Config) (*S3, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	err = client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{})
	if err != nil {
		// Ignore if the bucket already exists (idempotent).
		resp := minio.ToErrorResponse(err)
		if resp.Code != "BucketAlreadyOwnedByYou" && resp.Code != "BucketAlreadyExists" {
			return nil, err
		}
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3) Put(key string, r io.Reader) (Info, error) {
	// Buffer so we can compute checksums and supply a known size to PutObject.
	// TODO: replace with streaming multipart upload for large artifacts.
	var buf bytes.Buffer
	hSHA256 := sha256.New()
	hSHA1 := sha1.New() // #nosec G401
	hMD5 := md5.New()   // #nosec G401
	mw := io.MultiWriter(&buf, hSHA256, hSHA1, hMD5)
	n, err := io.Copy(mw, r)
	if err != nil {
		return Info{}, err
	}
	_, err = s.client.PutObject(
		context.Background(), s.bucket, key,
		bytes.NewReader(buf.Bytes()), n,
		minio.PutObjectOptions{},
	)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Size:   n,
		SHA256: hex.EncodeToString(hSHA256.Sum(nil)),
		SHA1:   hex.EncodeToString(hSHA1.Sum(nil)),
		MD5:    hex.EncodeToString(hMD5.Sum(nil)),
	}, nil
}

func (s *S3) Get(key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Trigger the actual request so a missing key fails now, not on first Read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *S3) Stat(key string) (Info, bool, error) {
	info, err := s.client.StatObject(context.Background(), s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return Info{}, false, nil
		}
		return Info{}, false, err
	}
	return Info{Size: info.Size}, true, nil
}

func (s *S3) List(prefix string) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(context.Background(), s.bucket,
		minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (s *S3) Delete(key string) error {
	err := s.client.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return nil
	}
	return err
}
