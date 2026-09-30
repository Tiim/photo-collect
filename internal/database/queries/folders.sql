-- name: CreateFolder :one
INSERT INTO folders (id, name, created_by) VALUES (?, ?, ?) RETURNING *;

-- name: GetFolder :one
SELECT * FROM folders WHERE id = ? AND deleted_at IS NULL;

-- name: GetFolderIncludingDeleted :one
SELECT * FROM folders WHERE id = ?;

-- name: ListFolders :many
SELECT f.id, f.name, f.created_at,
       (SELECT COUNT(*) FROM images i WHERE i.folder_id = f.id AND i.deleted_at IS NULL) AS image_count
FROM folders f
WHERE f.deleted_at IS NULL
ORDER BY f.created_at DESC, f.id;

-- name: RenameFolder :exec
UPDATE folders SET name = ? WHERE id = ? AND deleted_at IS NULL;

-- name: SoftDeleteFolder :execrows
UPDATE folders SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL;

-- name: HardDeleteFolder :exec
DELETE FROM folders WHERE id = ? AND deleted_at IS NOT NULL;

-- Visible (not trashed) images, for display.
-- name: CountImagesInFolder :one
SELECT COUNT(*) FROM images WHERE folder_id = ? AND deleted_at IS NULL;

-- All rows including the trash; trashed photos still occupy storage, so this
-- is what the per-folder capacity limit counts.
-- name: CountAllImagesInFolder :one
SELECT COUNT(*) FROM images WHERE folder_id = ?;
