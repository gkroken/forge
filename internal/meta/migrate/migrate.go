// Package migrate runs schema migrations for the Postgres meta.Store.
//
// Migrations are numbered SQL files embedded in this package. Up applies
// all unapplied migrations in order; Down rolls back all applied migrations
// in reverse order. Both are idempotent.
package migrate

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed sql/*.sql
var files embed.FS

type migration struct {
	version int
	up      string
	down    string
}

func load() ([]migration, error) {
	entries, err := files.ReadDir("sql")
	if err != nil {
		return nil, err
	}

	byVersion := map[int]*migration{}
	for _, e := range entries {
		name := e.Name() // e.g. "001_up.sql"
		parts := strings.SplitN(strings.TrimSuffix(name, ".sql"), "_", 2)
		if len(parts) != 2 {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(parts[0], "%d", &version); err != nil {
			continue
		}
		body, err := files.ReadFile("sql/" + name)
		if err != nil {
			return nil, err
		}
		if byVersion[version] == nil {
			byVersion[version] = &migration{version: version}
		}
		switch parts[1] {
		case "up":
			byVersion[version].up = string(body)
		case "down":
			byVersion[version].down = string(body)
		}
	}

	result := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		result = append(result, *m)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].version < result[j].version })
	return result, nil
}

func ensureTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	return err
}

func applied(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	done := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		done[v] = true
	}
	return done, rows.Err()
}

// Up applies all unapplied migrations in ascending version order.
func Up(db *sql.DB) error {
	if err := ensureTable(db); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}
	migrations, err := load()
	if err != nil {
		return fmt.Errorf("migrate: load: %w", err)
	}
	done, err := applied(db)
	if err != nil {
		return fmt.Errorf("migrate: check applied: %w", err)
	}
	for _, m := range migrations {
		if done[m.version] {
			continue
		}
		if _, err := db.Exec(m.up); err != nil {
			return fmt.Errorf("migrate: up %03d: %w", m.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			return fmt.Errorf("migrate: record %03d: %w", m.version, err)
		}
	}
	return nil
}

// Down rolls back all applied migrations in descending version order.
func Down(db *sql.DB) error {
	if err := ensureTable(db); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}
	migrations, err := load()
	if err != nil {
		return fmt.Errorf("migrate: load: %w", err)
	}
	done, err := applied(db)
	if err != nil {
		return fmt.Errorf("migrate: check applied: %w", err)
	}
	for i := len(migrations) - 1; i >= 0; i-- {
		m := migrations[i]
		if !done[m.version] {
			continue
		}
		// Unrecord BEFORE running the down SQL: the down SQL may drop
		// schema_migrations itself (as 001_down.sql does).
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = $1`, m.version); err != nil {
			return fmt.Errorf("migrate: unrecord %03d: %w", m.version, err)
		}
		if _, err := db.Exec(m.down); err != nil {
			return fmt.Errorf("migrate: down %03d: %w", m.version, err)
		}
	}
	return nil
}
