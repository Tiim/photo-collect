-- +goose Up
-- Share links: a read-only, time-limited link to a hand-picked set of a
-- folder's images for people without an account. A folder can have several.
CREATE TABLE shares (
    id         TEXT PRIMARY KEY,
    folder_id  TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    token      TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX shares_folder ON shares(folder_id);

CREATE TABLE share_images (
    share_id TEXT NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    image_id TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    PRIMARY KEY (share_id, image_id)
);
CREATE INDEX share_images_image ON share_images(image_id);

-- +goose Down
DROP TABLE share_images;
DROP TABLE shares;
