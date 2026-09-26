-- name: InsertImage :one
INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetImage :one
SELECT i.* FROM images i
JOIN folders f ON f.id = i.folder_id
WHERE i.id = ? AND f.deleted_at IS NULL;

-- name: ListImagesInFolder :many
SELECT * FROM images
WHERE folder_id = ? AND seq < sqlc.arg(before_seq)
ORDER BY seq DESC
LIMIT sqlc.arg(page_size);

-- name: ListImagesByIDs :many
SELECT * FROM images
WHERE folder_id = ? AND id IN (sqlc.slice(image_ids))
ORDER BY seq;

-- name: ListImageIDsInFolder :many
SELECT id FROM images WHERE folder_id = ? ORDER BY seq;

-- name: SetImageRating :execrows
UPDATE images SET rating = ? WHERE id = ?;

-- name: MarkThumbnailReady :exec
UPDATE images SET thumbnail_ready = 1 WHERE id = ?;

-- name: MarkPreviewReady :exec
UPDATE images SET preview_ready = 1 WHERE id = ?;

-- name: GetImageUnchecked :one
SELECT * FROM images WHERE id = ?;

-- name: ListExportImages :many
SELECT i.* FROM export_images ei
JOIN images i ON i.id = ei.image_id
WHERE ei.export_id = ?
ORDER BY i.seq;
