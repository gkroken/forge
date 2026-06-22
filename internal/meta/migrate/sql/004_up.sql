-- Shared last-run state for the cleanup scheduler. Under N replicas, the
-- scheduler uses a Postgres advisory lock to elect one leader per tick; this
-- table holds the per-repo last scheduled-run time so leadership can move
-- between replicas without re-firing a due job.
CREATE TABLE IF NOT EXISTS cleanup_schedule_state (
    repo     TEXT        PRIMARY KEY,
    last_run TIMESTAMPTZ NOT NULL
);
