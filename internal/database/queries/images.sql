-- name: InsertImage :one
INSERT INTO images (id, folder_id, original_filename, mime_type, size_bytes, width, height, sha256, uploader_nickname, device_key, exif_time)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
WHERE folder_id = ? AND device_key IS NOT NULL
GROUP BY device_key, uploader_nickname
ORDER BY uploader_nickname, device_key;
