package meta

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"forge/internal/meta/migrate"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver
)

// PG implements Store backed by a Postgres database.
// Schema is managed by migrate.Up, which runs automatically on construction.
type PG struct{ db *sql.DB }

// NewPG opens a connection to dsn, runs pending migrations, and returns a
// ready Store. dsn is a libpq-style connection string or URL.
func NewPG(dsn string) (*PG, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("meta.NewPG: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("meta.NewPG: ping: %w", err)
	}
	if err := migrate.Up(db); err != nil {
		return nil, fmt.Errorf("meta.NewPG: migrate: %w", err)
	}
	return &PG{db: db}, nil
}

// Close releases the database connection pool.
func (p *PG) Close() error { return p.db.Close() }

// DB returns the underlying *sql.DB so callers (e.g. queue.NewPG) can share
// the connection pool and benefit from the migrations already applied.
func (p *PG) DB() *sql.DB { return p.db }

func (p *PG) PutJSON(ns, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(
		`INSERT INTO meta (ns, key, val) VALUES ($1, $2, $3)
		 ON CONFLICT (ns, key) DO UPDATE SET val = EXCLUDED.val`,
		ns, key, b,
	)
	return err
}

func (p *PG) GetJSON(ns, key string, v any) (bool, error) {
	var raw []byte
	err := p.db.QueryRow(`SELECT val FROM meta WHERE ns=$1 AND key=$2`, ns, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, v)
}

func (p *PG) List(ns string) ([]string, error) {
	rows, err := p.db.Query(`SELECT key FROM meta WHERE ns=$1 ORDER BY key`, ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (p *PG) Delete(ns, key string) error {
	_, err := p.db.Exec(`DELETE FROM meta WHERE ns=$1 AND key=$2`, ns, key)
	return err
}
