-- name: UpsertTag :one
INSERT INTO tags (name) VALUES (?)
ON CONFLICT(name) DO UPDATE SET name = tags.name
RETURNING *;

-- name: SuggestTags :many
SELECT name FROM tags WHERE name LIKE sqlc.arg(prefix) || '%' ESCAPE '\' ORDER BY name LIMIT 20;

-- name: AddFolderStandardTag :exec
INSERT OR IGNORE INTO folder_standard_tags (folder_id, tag_id) VALUES (?, ?);

-- name: RemoveFolderStandardTag :exec
DELETE FROM folder_standard_tags WHERE folder_id = ? AND tag_id = ?;

-- name: ListFolderStandardTags :many
SELECT t.id, t.name FROM folder_standard_tags fst
JOIN tags t ON t.id = fst.tag_id
WHERE fst.folder_id = ?
ORDER BY t.name;

-- name: AddImageTag :exec
INSERT OR IGNORE INTO image_tags (image_id, tag_id) VALUES (?, ?);

-- name: RemoveImageTag :exec
DELETE FROM image_tags WHERE image_id = ? AND tag_id = ?;

-- name: ListImageTags :many
SELECT t.id, t.name FROM image_tags it
JOIN tags t ON t.id = it.tag_id
WHERE it.image_id = ?
ORDER BY t.name;

-- name: ListTagsForImages :many
SELECT it.image_id, t.name FROM image_tags it
JOIN tags t ON t.id = it.tag_id
WHERE it.image_id IN (sqlc.slice(image_ids))
ORDER BY it.image_id, t.name;

-- name: ListTagsForExport :many
SELECT it.image_id, t.name FROM export_images ei
JOIN image_tags it ON it.image_id = ei.image_id
JOIN tags t ON t.id = it.tag_id
WHERE ei.export_id = ?
ORDER BY it.image_id, t.name;
