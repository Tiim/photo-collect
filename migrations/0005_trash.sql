-- +goose Up
-- Soft delete: a non-NULL deleted_at puts the image in the folder's trash.
-- Trashed images keep their row and stored objects until they are purged.
ALTER TABLE images ADD COLUMN deleted_at TEXT;
ALTER TABLE images ADD COLUMN deleted_by TEXT; -- user id, informational (no foreign key)
CREATE INDEX images_folder_deleted ON images(folder_id, deleted_at);

-- +goose Down
DROP INDEX images_folder_deleted;
ALTER TABLE images DROP COLUMN deleted_by;
ALTER TABLE images DROP COLUMN deleted_at;
