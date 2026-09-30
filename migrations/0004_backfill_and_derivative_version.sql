-- +goose Up
-- Set once the derive job has finished with an image (success or permanent
-- failure), so the startup pHash backfill never re-enqueues it again.
ALTER TABLE images ADD COLUMN phash_attempted_at TEXT;
-- Bumped whenever preview/thumbnail are rewritten; part of the derivative ETag.
ALTER TABLE images ADD COLUMN derivative_version INTEGER NOT NULL DEFAULT 1;
-- Images that already have a hash were attempted.
UPDATE images SET phash_attempted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE phash IS NOT NULL;

-- +goose Down
ALTER TABLE images DROP COLUMN derivative_version;
ALTER TABLE images DROP COLUMN phash_attempted_at;
