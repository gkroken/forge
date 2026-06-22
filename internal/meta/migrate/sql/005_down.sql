DROP INDEX IF EXISTS jobs_visible_after_idx;
ALTER TABLE jobs DROP COLUMN IF EXISTS visible_after;
