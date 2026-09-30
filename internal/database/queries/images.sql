-- name: InsertImage :one
INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname, device_key, exif_time, gps_lat, gps_lon, gps_attempted_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- Trashed images (deleted_at IS NOT NULL) are invisible to every query below
-- unless its name says otherwise (Trashed, Any, All).

-- name: GetImage :one
SELECT i.* FROM images i
JOIN folders f ON f.id = i.folder_id
WHERE i.id = ? AND f.deleted_at IS NULL AND i.deleted_at IS NULL;

-- Includes trashed images (trash view thumbnails, background jobs).
-- name: GetImageAny :one
SELECT i.* FROM images i
JOIN folders f ON f.id = i.folder_id
WHERE i.id = ? AND f.deleted_at IS NULL;

-- name: ListImagesInFolder :many
SELECT * FROM images
WHERE folder_id = ? AND deleted_at IS NULL AND seq < sqlc.arg(before_seq)
ORDER BY seq DESC
LIMIT sqlc.arg(page_size);

-- name: ListTrashedImagesInFolder :many
SELECT * FROM images
WHERE folder_id = ? AND deleted_at IS NOT NULL AND seq < sqlc.arg(before_seq)
ORDER BY seq DESC
LIMIT sqlc.arg(page_size);

-- name: ListImagesByIDs :many
SELECT * FROM images
WHERE folder_id = ? AND deleted_at IS NULL AND id IN (sqlc.slice(image_ids))
ORDER BY seq;

-- name: ListTrashedImagesByIDs :many
SELECT * FROM images
WHERE folder_id = ? AND deleted_at IS NOT NULL AND id IN (sqlc.slice(image_ids))
ORDER BY seq;

-- name: TrashImages :execrows
UPDATE images SET deleted_at = sqlc.arg(deleted_at), deleted_by = sqlc.arg(deleted_by)
WHERE folder_id = sqlc.arg(folder_id) AND deleted_at IS NULL AND id IN (sqlc.slice(image_ids));

-- name: RestoreImages :execrows
UPDATE images SET deleted_at = NULL, deleted_by = NULL
WHERE folder_id = sqlc.arg(folder_id) AND deleted_at IS NOT NULL AND id IN (sqlc.slice(image_ids));

-- name: CountTrashedImagesInFolder :one
SELECT COUNT(*) FROM images WHERE folder_id = ? AND deleted_at IS NOT NULL;

-- Every image row, trashed or not, of folders that are not being deleted.
-- Used by the orphan sweeper.
-- name: ListAllImageRefs :many
SELECT i.id, i.folder_id FROM images i
JOIN folders f ON f.id = i.folder_id
WHERE f.deleted_at IS NULL;

-- name: ListDeletedFolderIDs :many
SELECT id FROM folders WHERE deleted_at IS NOT NULL;

-- name: SetImageRating :execrows
UPDATE images SET rating = ? WHERE id = ?;

-- name: MarkThumbnailReady :exec
UPDATE images SET thumbnail_ready = 1 WHERE id = ?;

-- name: MarkPreviewReady :exec
UPDATE images SET preview_ready = 1 WHERE id = ?;

-- name: GetImageUnchecked :one
SELECT * FROM images WHERE id = ?;

-- name: GetImageBySha256InFolder :one
SELECT * FROM images WHERE folder_id = ? AND sha256 = ?;

-- name: SetImagePHash :exec
UPDATE images SET phash = ?, phash_attempted_at = ? WHERE id = ?;

-- name: MarkPHashAttempted :exec
UPDATE images SET phash_attempted_at = ? WHERE id = ?;

-- name: BumpDerivativeVersion :exec
UPDATE images SET derivative_version = derivative_version + 1 WHERE id = ?;

-- name: ListAllImageHashesInFolder :many
SELECT id, phash FROM images WHERE folder_id = ? AND phash IS NOT NULL AND deleted_at IS NULL;

-- name: ListImagesMissingPHash :many
SELECT i.id, i.folder_id FROM images i
WHERE i.phash IS NULL AND i.phash_attempted_at IS NULL AND i.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.type = 'derive_image' AND j.status IN ('pending', 'running')
      AND json_extract(j.payload, '$.image_id') = i.id
  );

-- name: HardDeleteImage :execrows
DELETE FROM images WHERE id = ?;

-- name: ListExportImages :many
SELECT i.* FROM export_images ei
JOIN images i ON i.id = ei.image_id
WHERE ei.export_id = ? AND i.deleted_at IS NULL
ORDER BY i.seq;

-- name: MarkImageScanned :exec
UPDATE images SET qr_scanned = 1 WHERE id = ?;

-- name: SetImageCalibration :exec
UPDATE images SET is_calibration = 1, calib_ref_time = ? WHERE id = ?;

-- Recomputes the clock offset of every image of one device in a folder: each
-- image uses the calibration shot nearest to it in (uncorrected) EXIF time.
-- Images without any calibration for their device end up with NULL.
-- name: RecomputeDeviceOffsets :exec
UPDATE images SET time_offset_seconds = (
    SELECT CAST(strftime('%s', c.calib_ref_time) AS INTEGER) - CAST(strftime('%s', c.exif_time) AS INTEGER)
    FROM images c
    WHERE c.folder_id = images.folder_id AND c.device_key = images.device_key
      AND c.is_calibration = 1 AND c.exif_time IS NOT NULL AND c.calib_ref_time IS NOT NULL
    ORDER BY ABS(CAST(strftime('%s', c.exif_time) AS INTEGER) - CAST(strftime('%s', images.exif_time) AS INTEGER)), c.seq
    LIMIT 1)
WHERE images.folder_id = sqlc.arg(folder_id) AND images.device_key = sqlc.arg(device_key) AND images.exif_time IS NOT NULL;

-- name: ListFolderDevices :many
SELECT device_key, uploader_nickname,
       COUNT(*) AS image_count,
       CAST(COALESCE(SUM(is_calibration), 0) AS INTEGER) AS calibration_count,
       COUNT(time_offset_seconds) AS corrected_count,
       CAST(COALESCE(MIN(time_offset_seconds), 0) AS INTEGER) AS min_offset,
       CAST(COALESCE(MAX(time_offset_seconds), 0) AS INTEGER) AS max_offset
FROM images
WHERE folder_id = ? AND device_key IS NOT NULL AND deleted_at IS NULL
GROUP BY device_key, uploader_nickname
ORDER BY uploader_nickname, device_key;

-- Neighbours in upload order, for the previous/next links of the detail view.
-- name: PrevImageID :one
SELECT id FROM images
WHERE folder_id = ? AND deleted_at IS NULL AND seq < ?
ORDER BY seq DESC LIMIT 1;

-- name: NextImageID :one
SELECT id FROM images
WHERE folder_id = ? AND deleted_at IS NULL AND seq > ?
ORDER BY seq ASC LIMIT 1;
