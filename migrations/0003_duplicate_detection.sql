-- +goose Up
ALTER TABLE images ADD COLUMN phash INTEGER; -- 64-bit dHash, NULL until computed
-- UNIQUE (not just an index): makes the exact-duplicate skip on upload
-- race-safe under concurrent uploads of the same file within a folder.
CREATE UNIQUE INDEX images_folder_sha256 ON images(folder_id, sha256);

CREATE TABLE image_duplicates (
    id           TEXT    PRIMARY KEY,
    folder_id    TEXT    NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    image_id_a   TEXT    NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    image_id_b   TEXT    NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    distance     INTEGER NOT NULL, -- perceptual Hamming distance (0-64)
    status       TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'dismissed', 'resolved')),
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    resolved_at  TEXT,
    CHECK (image_id_a < image_id_b)
);
CREATE UNIQUE INDEX image_duplicates_pair ON image_duplicates(image_id_a, image_id_b);
CREATE INDEX image_duplicates_folder_status ON image_duplicates(folder_id, status);

-- +goose Down
DROP INDEX image_duplicates_folder_status;
DROP INDEX image_duplicates_pair;
DROP TABLE image_duplicates;
DROP INDEX images_folder_sha256;
ALTER TABLE images DROP COLUMN phash;
