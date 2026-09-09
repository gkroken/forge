package blob

import (
	"errors"
	"io"
)

// ErrImmutable is returned by an immutable Store's Put when the key already
// exists. Callers (format handlers) map it to HTTP 409 Conflict so a client
// re-publishing an existing artifact to a write-once repository gets a clear
// refusal instead of an opaque 500.
var ErrImmutable = errors.New("immutable repository: artifacts are write-once and cannot be overwritten or deleted")

// immutable is a write-once wrapper around a Store: Put fails with ErrImmutable
// when the target key already holds bytes, and Delete fails the same way, so an
// existing artifact can never be replaced — by overwriting it or by removing it
// and writing again. Reads, stats and lists pass straight through.
//
// Delete used to pass through too, on the stated assumption that "the server
// blocks soft-delete on immutable repos elsewhere". That was true only of the
// admin API: a protocol-level DELETE /repository/{repo}/{path} went through the
// format handler to the store and succeeded, after which the coordinate could
// be re-published with different bytes. Write-once has to hold at the layer
// that owns the bytes, not at one of the routes that reach it.
//
// It is applied per-request in format.Context.Blob for hosted repositories with
// Immutable set. Because it guards at the key level it is format-agnostic — the
// storage layer still knows nothing about Maven vs npm — but it must not wrap
// OCI repositories, whose content-addressed layers are legitimately re-pushed
// (a shared layer already present is a normal, idempotent write, not an
// overwrite). The server only wraps the file formats for that reason.
type immutable struct{ Store }

// Immutable returns a write-once view of s. New keys are written normally;
// writing a key that already exists returns ErrImmutable.
func Immutable(s Store) Store { return immutable{s} }

func (i immutable) Put(key string, r io.Reader) (Info, error) {
	if _, exists, err := i.Store.Stat(key); err == nil && exists {
		return Info{}, ErrImmutable
	}
	return i.Store.Put(key, r)
}

// Delete refuses removal: allowing it would let a caller delete an artifact and
// re-publish the same coordinate with different content, which is exactly the
// mutation this wrapper exists to prevent.
func (i immutable) Delete(key string) error {
	if _, exists, err := i.Store.Stat(key); err == nil && exists {
		return ErrImmutable
	}
	return i.Store.Delete(key)
}
