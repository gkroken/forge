package blob

import (
	"errors"
	"io"
)

// ErrImmutable is returned by an immutable Store's Put when the key already
// exists. Callers (format handlers) map it to HTTP 409 Conflict so a client
// re-publishing an existing artifact to a write-once repository gets a clear
// refusal instead of an opaque 500.
var ErrImmutable = errors.New("immutable repository: artifact already exists and cannot be overwritten")

// immutable is a write-once wrapper around a Store: Put fails with ErrImmutable
// when the target key already holds bytes, so an existing artifact can never be
// silently replaced. Every other operation passes straight through — reads,
// stats, lists and deletes are unaffected (deletion is a separate policy; the
// server blocks soft-delete on immutable repos elsewhere).
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
