// Package meta stores structured metadata that doesn't live naturally in the
// blob layout: npm packuments, Helm chart records, proxy cache entries, etc.
//
// The prototype uses one JSON file per record. Production swaps in Postgres
// behind the same interface (each namespace becomes a table).
package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is a namespaced JSON document store.
type Store interface {
	GetJSON(ns, key string, v any) (bool, error)
	PutJSON(ns, key string, v any) error
	List(ns string) ([]string, error)
	Delete(ns, key string) error
}

// FS implements Store with one .json file per record under root/ns/key.json.
type FS struct{ root string }

func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o750); err != nil { // #nosec G301
		return nil, err
	}
	return &FS{root: root}, nil
}

// resolve returns the absolute file path for (ns, key), rejecting any input
// that would escape the store root. key's slashes are replaced with __ to
// flatten the key into a filename component.
func (f *FS) resolve(ns, key string) (string, error) {
	safe := strings.ReplaceAll(key, "/", "__")
	full := filepath.Join(f.root, ns, safe+".json")
	root := filepath.Clean(f.root)
	if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal meta key ns=%q key=%q", ns, key)
	}
	return full, nil
}

// resolveDir returns the absolute directory path for ns, rejecting traversal.
func (f *FS) resolveDir(ns string) (string, error) {
	dir := filepath.Join(f.root, ns)
	root := filepath.Clean(f.root)
	if dir != root && !strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("illegal meta namespace %q", ns)
	}
	return dir, nil
}

func (f *FS) GetJSON(ns, key string, v any) (bool, error) {
	p, err := f.resolve(ns, key)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(p) // #nosec G304 -- path sanitised by resolve()
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (f *FS) PutJSON(ns, key string, v any) error {
	p, err := f.resolve(ns, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil { // #nosec G301
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600) // #nosec G306
}

func (f *FS) List(ns string) ([]string, error) {
	dir, err := f.resolveDir(ns)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		keys = append(keys, strings.ReplaceAll(name, "__", "/"))
	}
	return keys, nil
}

func (f *FS) Delete(ns, key string) error {
	p, err := f.resolve(ns, key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
