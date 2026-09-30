-- name: CreateExport :one
INSERT INTO exports (id, folder_id, user_id, expires_at) VALUES (?, ?, ?, ?) RETURNING *;

-- name: AddExportImage :exec
INSERT OR IGNORE INTO export_images (export_id, image_id) VALUES (?, ?);

-- name: AddAllFolderImagesToExport :exec
INSERT INTO export_images (export_id, image_id)
SELECT sqlc.arg(export_id), id FROM images WHERE folder_id = sqlc.arg(folder_id) AND is_calibration = 0;

-- name: GetExport :one
SELECT * FROM exports WHERE id = ?;

-- name: ListFolderExports :many
SELECT * FROM exports WHERE folder_id = ? ORDER BY created_at DESC LIMIT 20;

-- name: MarkExportRunning :exec
UPDATE exports SET status = 'running' WHERE id = ?;

-- name: MarkExportReady :exec
UPDATE exports SET status = 'ready', file_key = ?, size_bytes = ?, error = NULL WHERE id = ?;

-- name: MarkExportFailed :exec
UPDATE exports SET status = 'failed', error = ? WHERE id = ?;

-- name: ListExpiredExports :many
SELECT * FROM exports WHERE expires_at <= ?;

-- name: DeleteExport :exec
DELETE FROM exports WHERE id = ?;

-- name: ListFolderExportKeys :many
SELECT id, file_key FROM exports WHERE folder_id = ? AND file_key IS NOT NULL;

-- name: SumExportImageBytes :one
SELECT CAST(COALESCE(SUM(i.size_bytes), 0) AS INTEGER) FROM export_images ei
JOIN images i ON i.id = ei.image_id WHERE ei.export_id = ?;
