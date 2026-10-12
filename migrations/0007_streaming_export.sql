-- +goose Up
-- File name of the image in exports (ZIP and "save to folder"). Assigned once,
-- in upload order, the first time the folder is exported, and never changed, so
-- repeated exports of the same image use the same name. Unique per folder
-- (case-insensitively, also without extension so that XMP sidecars cannot
-- collide); see domain.UniqueName.
ALTER TABLE images ADD COLUMN export_name TEXT;
CREATE UNIQUE INDEX images_folder_export_name ON images(folder_id, export_name COLLATE NOCASE);

-- ZIP exports are streamed now: the background build and its bookkeeping are gone.
DELETE FROM jobs WHERE type = 'build_export';
DROP TABLE export_images;
DROP TABLE exports;

-- +goose Down
CREATE TABLE exports (
    id          TEXT PRIMARY KEY,
    folder_id   TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    user_id     TEXT REFERENCES users(id) ON DELETE SET NULL,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'ready', 'failed')),
    file_key    TEXT,
    size_bytes  INTEGER,
    error       TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at  TEXT NOT NULL
);
CREATE TABLE export_images (
    export_id TEXT NOT NULL REFERENCES exports(id) ON DELETE CASCADE,
    image_id  TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    PRIMARY KEY (export_id, image_id)
);
DROP INDEX images_folder_export_name;
ALTER TABLE images DROP COLUMN export_name;
