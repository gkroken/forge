CREATE TABLE IF NOT EXISTS audit_log (
    id      BIGSERIAL    PRIMARY KEY,
    ts      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    actor   TEXT         NOT NULL,
    method  TEXT         NOT NULL,
    path    TEXT         NOT NULL,
    status  INTEGER      NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_log_ts_idx ON audit_log (ts DESC, id DESC);
