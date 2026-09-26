-- name: EnqueueJob :one
INSERT INTO jobs (type, payload, run_at) VALUES (?, ?, ?) RETURNING id;

-- name: ClaimJob :one
UPDATE jobs
SET status = 'running', attempts = attempts + 1, started_at = sqlc.arg(now)
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.status = 'pending' AND j.run_at <= sqlc.arg(now)
    ORDER BY j.run_at, j.id
    LIMIT 1
)
RETURNING *;

-- name: CompleteJob :exec
UPDATE jobs SET status = 'done', finished_at = ?, error = NULL WHERE id = ?;

-- name: RetryJobLater :exec
UPDATE jobs SET status = 'pending', run_at = ?, error = ?, started_at = NULL WHERE id = ?;

-- name: FailJob :exec
UPDATE jobs SET status = 'failed', finished_at = ?, error = ? WHERE id = ?;

-- name: RequeueStaleJobs :execrows
UPDATE jobs SET status = 'pending', started_at = NULL
WHERE status = 'running' AND started_at < ?;

-- name: ManualRetryJob :execrows
UPDATE jobs SET status = 'pending', attempts = 0, run_at = ?, finished_at = NULL WHERE id = ? AND status = 'failed';

-- name: ListFailedJobs :many
SELECT * FROM jobs WHERE status = 'failed' ORDER BY id DESC LIMIT 100;

-- name: DeleteOldJobs :exec
DELETE FROM jobs WHERE status = 'done' AND finished_at < ?;
