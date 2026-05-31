//go:build integration

package meta_test

import (
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"forge/internal/meta"
	"forge/internal/meta/metatest"
	"forge/internal/meta/migrate"
	"forge/internal/testutil"
)

func TestPG_Contract(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	s, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	metatest.RunContract(t, s)
}

// TestPG_MigrateUpDown verifies that Down wipes all data and Up re-creates a
// clean schema, which is the basic rollback contract required by the workplan.
func TestPG_MigrateUpDown(t *testing.T) {
	dsn := testutil.StartPostgres(t)

	// Bring up the schema and write a record.
	s, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	if err := s.PutJSON("ns", "k", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	s.Close()

	// Roll back and re-apply using the raw DB handle.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate.Down(db); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up after down: %v", err)
	}

	// The record written before Down must be gone.
	s2, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG after down/up: %v", err)
	}
	defer s2.Close()
	var v map[string]string
	ok, err := s2.GetJSON("ns", "k", &v)
	if err != nil || ok {
		t.Fatalf("expected record gone after Down, got ok=%v err=%v", ok, err)
	}
}

// TestPG_MigrateUpDown_ProdSized seeds a production-shaped dataset before
// rolling back, verifying that Down clears all data regardless of volume.
// Mimics: 10 repos × 50 packages = 500 metadata records.
func TestPG_MigrateUpDown_ProdSized(t *testing.T) {
	const repos, pkgs = 10, 50
	dsn := testutil.StartPostgres(t)

	s, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG: %v", err)
	}
	for i := range repos {
		ns := fmt.Sprintf("repo-%02d:npm", i)
		for j := range pkgs {
			key := fmt.Sprintf("pkg-%04d", j)
			if err := s.PutJSON(ns, key, map[string]any{
				"name": key, "version": "1.0.0",
				"dist": map[string]any{"tarball": "http://localhost/" + key},
			}); err != nil {
				t.Fatalf("seed %s/%s: %v", ns, key, err)
			}
		}
	}
	s.Close()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	db.QueryRow("SELECT COUNT(*) FROM meta").Scan(&count) //nolint:errcheck
	if count != repos*pkgs {
		t.Fatalf("expected %d records before Down, got %d", repos*pkgs, count)
	}

	if err := migrate.Down(db); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up after down: %v", err)
	}

	s2, err := meta.NewPG(dsn)
	if err != nil {
		t.Fatalf("NewPG after down/up: %v", err)
	}
	defer s2.Close()
	for i := range repos {
		ns := fmt.Sprintf("repo-%02d:npm", i)
		keys, err := s2.List(ns)
		if err != nil {
			t.Fatalf("list %s: %v", ns, err)
		}
		if len(keys) != 0 {
			t.Fatalf("namespace %s not empty after Down+Up: %d records", ns, len(keys))
		}
	}
}
