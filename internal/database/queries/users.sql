-- name: UpsertUser :one
INSERT INTO users (id, oidc_sub, email, name)
VALUES (?, ?, ?, ?)
ON CONFLICT(oidc_sub) DO UPDATE SET email = excluded.email, name = excluded.name
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;
