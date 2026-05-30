// Package blob is the storage abstraction for raw artifact bytes.
//
// The prototype ships a filesystem implementation. In production you would add
// an S3 implementation behind the same interface; nothing above this layer
// needs to change. Keys are logical paths, e.g. "maven-hosted/com/acme/lib/1.0/lib-1.0.jar".
package blob

import "io"

// Info carries the size and checksums computed while writing a blob.
// Maven clients require md5 + sha1 sidecars; everything modern wants sha256.
type Info struct {
	Size   int64
	SHA256 string
	SHA1   string
	MD5    string
}

// Store is the contract every backend (filesystem, S3, GCS, ...) must satisfy.
type Store interface {
	// Put streams r into key, returning computed size + checksums.
	Put(key string, r io.Reader) (Info, error)
	// Get opens key for reading. Caller must Close.
	Get(key string) (io.ReadCloser, error)
	// Stat reports whether key exists and its Info (checksums may be empty for fs).
	Stat(key string) (Info, bool, error)
	// List returns all keys under prefix (recursive).
	List(prefix string) ([]string, error)
	// Delete removes key. No error if absent.
	Delete(key string) error
}
