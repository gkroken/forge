// Package meta stores structured metadata that doesn't live naturally in the
// blob layout: npm packuments, Helm chart records, proxy cache entries, etc.
//
// The prototype uses one JSON file per record. Production swaps in Postgres
// behind the same interface (each namespace becomes a table).
package meta

import (
	"encoding/json"
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &FS{root: root}, nil
}

func (f *FS) path(ns, key string) string {
	safe := strings.ReplaceAll(key, "/", "__")
	return filepath.Join(f.root, ns, safe+".json")
}

func (f *FS) GetJSON(ns, key string, v any) (bool, error) {
	b, err := os.ReadFile(f.path(ns, key))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (f *FS) PutJSON(ns, key string, v any) error {
	p := f.path(ns, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func (f *FS) List(ns string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, ns))
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
	err := os.Remove(f.path(ns, key))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
