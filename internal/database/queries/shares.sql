-- name: CreateShare :one
INSERT INTO shares (id, folder_id, token, name, label, expires_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetShare :one
SELECT s.* FROM shares s
JOIN folders f ON f.id = s.folder_id
WHERE s.id = ? AND f.deleted_at IS NULL;

-- name: GetShareByToken :one
SELECT s.* FROM shares s
JOIN folders f ON f.id = s.folder_id
WHERE s.token = ? AND f.deleted_at IS NULL;

-- Shares of a folder with the number of (not trashed) images in each.
-- name: ListFolderShares :many
SELECT s.id, s.name, s.label, s.expires_at,
       (SELECT COUNT(*) FROM share_images si JOIN images i ON i.id = si.image_id
        WHERE si.share_id = s.id AND i.deleted_at IS NULL) AS image_count
FROM shares s
WHERE s.folder_id = ?
ORDER BY s.created_at DESC, s.id;

-- name: RenameShare :execrows
UPDATE shares SET name = ?, label = ? WHERE id = ?;

-- name: ExtendShare :execrows
UPDATE shares SET expires_at = ? WHERE id = ?;

-- name: DeleteShare :exec
DELETE FROM shares WHERE id = ?;

-- name: DeleteFolderShares :exec
DELETE FROM shares WHERE folder_id = ?;

-- Adds the folder's (not trashed) images among image_ids; others are ignored.
-- name: AddShareImages :execrows
INSERT OR IGNORE INTO share_images (share_id, image_id)
SELECT sqlc.arg(share_id), id FROM images
WHERE folder_id = sqlc.arg(folder_id) AND deleted_at IS NULL AND id IN (sqlc.slice(image_ids));

-- name: RemoveShareImages :execrows
DELETE FROM share_images WHERE share_id = sqlc.arg(share_id) AND image_id IN (sqlc.slice(image_ids));

-- name: CountShareImages :one
SELECT COUNT(*) FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = ? AND i.deleted_at IS NULL;

-- One page of a share's images in upload order, after the given seq.
-- name: ListShareImages :many
SELECT i.* FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = sqlc.arg(share_id) AND i.deleted_at IS NULL AND i.seq > sqlc.arg(after_seq)
ORDER BY i.seq
LIMIT sqlc.arg(page_size);

-- name: ListShareImageIDs :many
SELECT i.id FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = ? AND i.deleted_at IS NULL
ORDER BY i.seq;

-- name: GetShareImage :one
SELECT i.* FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = ? AND si.image_id = ? AND i.deleted_at IS NULL;

-- name: PrevShareImageID :one
SELECT i.id FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = ? AND i.deleted_at IS NULL AND i.seq < ?
ORDER BY i.seq DESC LIMIT 1;

-- name: NextShareImageID :one
SELECT i.id FROM share_images si
JOIN images i ON i.id = si.image_id
WHERE si.share_id = ? AND i.deleted_at IS NULL AND i.seq > ?
ORDER BY i.seq LIMIT 1;
