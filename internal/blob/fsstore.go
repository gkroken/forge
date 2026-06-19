package blob

import (
	"crypto/md5"    // #nosec G501 -- MD5/SHA1 required by Maven/npm protocol specs
	"crypto/sha1"   // #nosec G505
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed Store rooted at a directory.
type FS struct {
	root string
}

// NewFS creates the root directory if needed and returns a filesystem store.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o750); err != nil { // #nosec G301
		return nil, err
	}
	return &FS{root: root}, nil
}

// resolve maps a logical key to an absolute path, rejecting traversal attempts.
func (f *FS) resolve(key string) (string, error) {
	clean := filepath.Clean("/" + strings.TrimPrefix(key, "/"))
	full := filepath.Join(f.root, clean)
	if !strings.HasPrefix(full, filepath.Clean(f.root)+string(os.PathSeparator)) && full != f.root {
		return "", fmt.Errorf("illegal key %q", key)
	}
	return full, nil
}

func (f *FS) Put(key string, r io.Reader) (Info, error) {
	path, err := f.resolve(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { // #nosec G301
		return Info{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return Info{}, err
	}
	defer os.Remove(tmp.Name())

	hSHA256 := sha256.New()
	hSHA1 := sha1.New() // #nosec G401
	hMD5 := md5.New()   // #nosec G401
	mw := io.MultiWriter(tmp, hSHA256, hSHA1, hMD5)

	n, err := io.Copy(mw, r)
	if err != nil {
		tmp.Close()
		return Info{}, err
	}
	if err := tmp.Close(); err != nil {
		return Info{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Info{}, err
	}
	return Info{
		Size:   n,
		SHA256: hex.EncodeToString(hSHA256.Sum(nil)),
		SHA1:   hex.EncodeToString(hSHA1.Sum(nil)),
		MD5:    hex.EncodeToString(hMD5.Sum(nil)),
	}, nil
}

func (f *FS) Get(key string) (io.ReadCloser, error) {
	path, err := f.resolve(key)
	if err != nil {
		return nil, err
	}
	return os.Open(path) // #nosec G304 -- path sanitised by resolve()
}

func (f *FS) Stat(key string) (Info, bool, error) {
	path, err := f.resolve(key)
	if err != nil {
		return Info{}, false, err
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return Info{}, false, nil
	}
	if err != nil {
		return Info{}, false, err
	}
	return Info{Size: st.Size(), ModTime: st.ModTime()}, true, nil
}

func (f *FS) List(prefix string) ([]string, error) {
	base, err := f.resolve(prefix)
	if err != nil {
		return nil, err
	}
	var keys []string
	_ = filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), ".upload-") {
			return nil
		}
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			return nil
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	return keys, nil
}

func (f *FS) Delete(key string) error {
	path, err := f.resolve(key)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// HashReader is a small helper to checksum an in-memory byte slice.
func sum(h hash.Hash, b []byte) string {
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256 / SHA1 / MD5 of a byte slice (used by format handlers for sidecars).
func SHA256(b []byte) string { return sum(sha256.New(), b) }
func SHA1(b []byte) string { return sum(sha1.New(), b) } // #nosec G401
func MD5(b []byte) string  { return sum(md5.New(), b) }  // #nosec G401
