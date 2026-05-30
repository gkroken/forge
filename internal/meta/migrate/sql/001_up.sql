CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS meta (
    ns   TEXT NOT NULL,
    key  TEXT NOT NULL,
    val  JSONB NOT NULL,
    PRIMARY KEY (ns, key)
);
