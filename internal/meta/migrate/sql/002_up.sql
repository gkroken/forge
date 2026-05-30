CREATE TABLE IF NOT EXISTS jobs (
    id         BIGSERIAL    PRIMARY KEY,
    type       TEXT         NOT NULL,
    payload    JSONB        NOT NULL,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
