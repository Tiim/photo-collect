-- name: InsertImageDuplicate :exec
INSERT INTO image_duplicates (id, folder_id, image_id_a, image_id_b, distance)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (image_id_a, image_id_b) DO NOTHING;

-- name: ListPendingDuplicates :many
SELECT d.*,
       a.original_filename AS a_filename,
       b.original_filename AS b_filename
FROM image_duplicates d
JOIN images a ON a.id = d.image_id_a
JOIN images b ON b.id = d.image_id_b
WHERE d.folder_id = ? AND d.status = 'pending'
  AND a.deleted_at IS NULL AND b.deleted_at IS NULL
ORDER BY d.created_at;

-- name: GetImageDuplicate :one
SELECT * FROM image_duplicates WHERE id = ?;

-- name: DismissImageDuplicate :execrows
UPDATE image_duplicates SET status = 'dismissed', resolved_at = ? WHERE id = ? AND status = 'pending';

-- name: ResolveImageDuplicate :execrows
UPDATE image_duplicates SET status = 'resolved', resolved_at = ? WHERE id = ? AND status = 'pending';

-- name: ResolveDuplicatesForImage :exec
UPDATE image_duplicates SET status = 'resolved', resolved_at = ?
WHERE (image_id_a = ? OR image_id_b = ?) AND status = 'pending';
