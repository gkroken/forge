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

// s3InlineMax is the largest artifact uploaded in a single request with a known
// length. Below it the body is small enough to hold briefly; above it, memory
// matters more than the extra round trips.
//
// s3PartSize bounds what minio-go allocates for an unknown-length upload. It
// must be at least S3's 5 MiB part minimum.
const (
	s3InlineMax = 8 << 20
	s3PartSize  = 16 << 20
)

func (s *S3) Put(key string, r io.Reader) (Info, error) {
	hSHA256 := sha256.New()
	hSHA1 := sha1.New() // #nosec G401
	hMD5 := md5.New()   // #nosec G401
	hashes := io.MultiWriter(hSHA256, hSHA1, hMD5)

	// Read up to the inline limit to find out which kind of upload this is.
	// Nearly every artifact — a jar, a chart, an npm tarball — ends here, and
	// gets a single known-length PUT whose memory cost is its own size.
	var head bytes.Buffer
	n, err := io.CopyN(&head, r, s3InlineMax+1)
	if err != nil && err != io.EOF {
		return Info{}, err
	}

	var info minio.UploadInfo
	if n <= s3InlineMax {
		data := head.Bytes()
		if _, err := hashes.Write(data); err != nil {
			return Info{}, err
		}
		info, err = s.client.PutObject(
			context.Background(), s.bucket, key,
			bytes.NewReader(data), int64(len(data)),
			minio.PutObjectOptions{},
		)
	} else {
		// Genuinely large: stream the rest rather than buffering it, and pin the
		// part size so an upload costs one bounded buffer instead of whatever
		// minio-go would pick for an object of unknown length. Passing -1 here
		// without a PartSize is what OOM-killed a 512Mi pod: every concurrent
		// upload, however small the artifact, reserved a part buffer.
		body := io.MultiReader(bytes.NewReader(head.Bytes()), r)
		info, err = s.client.PutObject(
			context.Background(), s.bucket, key,
			io.TeeReader(body, hashes), -1,
			minio.PutObjectOptions{PartSize: s3PartSize},
		)
	}
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
