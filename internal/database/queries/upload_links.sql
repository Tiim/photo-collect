-- name: UpsertUploadLink :one
INSERT INTO upload_links (folder_id, token, expires_at)
VALUES (?, ?, ?)
ON CONFLICT(folder_id) DO UPDATE SET
    token = excluded.token,
    expires_at = excluded.expires_at,
    created_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
RETURNING *;

-- name: ExtendUploadLink :execrows
UPDATE upload_links SET expires_at = ? WHERE folder_id = ?;

-- name: GetUploadLinkByFolder :one
SELECT * FROM upload_links WHERE folder_id = ?;

-- name: GetUploadLinkByToken :one
SELECT ul.folder_id, ul.token, ul.expires_at, f.name AS folder_name
FROM upload_links ul
JOIN folders f ON f.id = ul.folder_id
WHERE ul.token = ? AND f.deleted_at IS NULL;

-- name: DeleteUploadLink :exec
DELETE FROM upload_links WHERE folder_id = ?;
