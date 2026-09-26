-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at)
VALUES (?, ?, ?, ?);

-- name: GetSession :one
SELECT s.token_hash, s.csrf_token, s.expires_at,
       u.id AS user_id, u.email, u.name
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > sqlc.arg(now);

-- name: ExtendSession :exec
UPDATE sessions SET expires_at = ? WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= ?;
