package blob

import (
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
	// Stream straight through to S3, hashing as the bytes go past, rather than
	// buffering the whole artifact to learn its size first: a multi-gigabyte
	// container layer or fat jar would otherwise be held entirely in memory,
	// and several concurrent uploads would exhaust the process.
	//
	// Size -1 tells minio-go the length is unknown, which makes it choose a
	// multipart upload and stream the parts. The TeeReader feeds every byte to
	// the checksum hashes on its way into that upload, so the digests are
	// complete exactly when the upload is.
	hSHA256 := sha256.New()
	hSHA1 := sha1.New() // #nosec G401
	hMD5 := md5.New()   // #nosec G401
	tee := io.TeeReader(r, io.MultiWriter(hSHA256, hSHA1, hMD5))

	info, err := s.client.PutObject(
		context.Background(), s.bucket, key,
		tee, -1,
		minio.PutObjectOptions{},
	)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Size:   info.Size,
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
