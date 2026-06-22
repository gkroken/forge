-- Delayed-visibility column for the job queue: a job is only eligible to be
-- dequeued once visible_after has passed. Backs delayed retries (e.g. webhook
-- delivery backoff). Existing rows default to now() = immediately eligible.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS visible_after TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS jobs_visible_after_idx ON jobs (visible_after, id);
