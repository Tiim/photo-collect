-- +goose Up
CREATE TABLE users (
    id         TEXT PRIMARY KEY,
    oidc_sub   TEXT NOT NULL UNIQUE,
    email      TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT NOT NULL
);
CREATE INDEX sessions_expires_at ON sessions(expires_at);

CREATE TABLE folders (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    deleted_at TEXT
);

CREATE TABLE tags (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL COLLATE NOCASE UNIQUE
);

CREATE TABLE folder_standard_tags (
    folder_id TEXT    NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    tag_id    INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (folder_id, tag_id)
);

CREATE TABLE upload_links (
    folder_id  TEXT PRIMARY KEY REFERENCES folders(id) ON DELETE CASCADE,
    token      TEXT NOT NULL UNIQUE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE images (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT    NOT NULL UNIQUE,
    folder_id         TEXT    NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    original_filename TEXT    NOT NULL,
    mime_type         TEXT    NOT NULL,
    size_bytes        INTEGER NOT NULL,
    width             INTEGER NOT NULL,
    height            INTEGER NOT NULL,
    sha256            TEXT    NOT NULL,
    uploader_nickname TEXT    NOT NULL,
    rating            INTEGER CHECK (rating IS NULL OR rating BETWEEN 1 AND 5),
    thumbnail_ready   INTEGER NOT NULL DEFAULT 0,
    preview_ready     INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX images_folder_seq ON images(folder_id, seq);

CREATE TABLE image_tags (
    image_id TEXT    NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    tag_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (image_id, tag_id)
);
CREATE INDEX image_tags_tag ON image_tags(tag_id);

CREATE TABLE jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    type        TEXT    NOT NULL,
    payload     TEXT    NOT NULL DEFAULT '{}',
    status      TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0,
    run_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at  TEXT,
    finished_at TEXT,
    error       TEXT
);
CREATE INDEX jobs_claim ON jobs(status, run_at);

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

-- +goose Down
DROP TABLE export_images;
DROP TABLE exports;
DROP TABLE jobs;
DROP TABLE image_tags;
DROP TABLE images;
DROP TABLE upload_links;
DROP TABLE folder_standard_tags;
DROP TABLE tags;
DROP TABLE folders;
DROP TABLE sessions;
DROP TABLE users;
