-- The gallery filter. Every filter is an optional argument (NULL = not set);
-- internal/filter maps a parsed query string onto these parameters.
--
--   tags         JSON array of tag names; an image needs `tags_needed` of them
--                (all of them for "all", 1 for "any").
--   from_time / to_time
--                bounds on the corrected capture time, "YYYY-MM-DD HH:MM:SS".
--   sort         'uploaded' | 'captured' | 'rating'; dir 'asc' | 'desc'.
--   after_key / after_seq
--                keyset cursor: the sort_key and seq of the last row of the
--                previous page (both NULL for the first page).
--
-- The three queries below share one predicate; keep them identical.

-- name: ListImagesFiltered :many
SELECT * FROM (
  SELECT i.*, CASE CAST(sqlc.arg(sort) AS TEXT)
      WHEN 'captured' THEN COALESCE(datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))), '')
      WHEN 'rating' THEN printf('%03d', COALESCE(i.rating, 0))
      ELSE printf('%020d', i.seq) END AS sort_key,
         CAST(sqlc.arg(dir) AS TEXT) = 'desc' AS is_desc
  FROM images i
  WHERE i.folder_id = sqlc.arg(folder_id)
  AND i.deleted_at IS NULL
  AND (CAST(sqlc.narg(rating_min) AS INTEGER) IS NULL OR i.rating >= CAST(sqlc.narg(rating_min) AS INTEGER))
  AND (CAST(sqlc.narg(rating_max) AS INTEGER) IS NULL OR i.rating <= CAST(sqlc.narg(rating_max) AS INTEGER))
  AND (CAST(sqlc.narg(uploader) AS TEXT) IS NULL OR i.uploader_nickname = CAST(sqlc.narg(uploader) AS TEXT))
  AND (CAST(sqlc.narg(from_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) >= CAST(sqlc.narg(from_time) AS TEXT))
  AND (CAST(sqlc.narg(to_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) <= CAST(sqlc.narg(to_time) AS TEXT))
  AND (CAST(sqlc.narg(has_gps) AS INTEGER) IS NULL OR i.gps_lat IS NOT NULL)
  AND (CAST(sqlc.narg(tags) AS TEXT) IS NULL OR (
        SELECT COUNT(DISTINCT t.id) FROM image_tags it JOIN tags t ON t.id = it.tag_id
        WHERE it.image_id = i.id AND t.name IN (SELECT value FROM json_each(CAST(sqlc.narg(tags) AS TEXT)))
      ) >= sqlc.arg(tags_needed))
) AS f
WHERE CAST(sqlc.narg(after_seq) AS INTEGER) IS NULL
   OR (NOT f.is_desc AND (f.sort_key, f.seq) > (CAST(sqlc.narg(after_key) AS TEXT), CAST(sqlc.narg(after_seq) AS INTEGER)))
   OR (f.is_desc AND (f.sort_key, f.seq) < (CAST(sqlc.narg(after_key) AS TEXT), CAST(sqlc.narg(after_seq) AS INTEGER)))
ORDER BY CASE WHEN NOT f.is_desc THEN f.sort_key END ASC,
         CASE WHEN f.is_desc THEN f.sort_key END DESC,
         CASE WHEN NOT f.is_desc THEN f.seq END ASC,
         CASE WHEN f.is_desc THEN f.seq END DESC
LIMIT sqlc.arg(page_size);

-- name: CountImagesFiltered :one
SELECT COUNT(*) FROM images i
WHERE i.folder_id = sqlc.arg(folder_id)
  AND i.deleted_at IS NULL
  AND (CAST(sqlc.narg(rating_min) AS INTEGER) IS NULL OR i.rating >= CAST(sqlc.narg(rating_min) AS INTEGER))
  AND (CAST(sqlc.narg(rating_max) AS INTEGER) IS NULL OR i.rating <= CAST(sqlc.narg(rating_max) AS INTEGER))
  AND (CAST(sqlc.narg(uploader) AS TEXT) IS NULL OR i.uploader_nickname = CAST(sqlc.narg(uploader) AS TEXT))
  AND (CAST(sqlc.narg(from_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) >= CAST(sqlc.narg(from_time) AS TEXT))
  AND (CAST(sqlc.narg(to_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) <= CAST(sqlc.narg(to_time) AS TEXT))
  AND (CAST(sqlc.narg(has_gps) AS INTEGER) IS NULL OR i.gps_lat IS NOT NULL)
  AND (CAST(sqlc.narg(tags) AS TEXT) IS NULL OR (
        SELECT COUNT(DISTINCT t.id) FROM image_tags it JOIN tags t ON t.id = it.tag_id
        WHERE it.image_id = i.id AND t.name IN (SELECT value FROM json_each(CAST(sqlc.narg(tags) AS TEXT)))
      ) >= sqlc.arg(tags_needed));

-- Ids (and positions, for the map) of every match in upload order, at most `max_rows`.
-- name: ListFilteredRefs :many
SELECT i.id, i.gps_lat, i.gps_lon FROM images i
WHERE i.folder_id = sqlc.arg(folder_id)
  AND i.deleted_at IS NULL
  AND (CAST(sqlc.narg(rating_min) AS INTEGER) IS NULL OR i.rating >= CAST(sqlc.narg(rating_min) AS INTEGER))
  AND (CAST(sqlc.narg(rating_max) AS INTEGER) IS NULL OR i.rating <= CAST(sqlc.narg(rating_max) AS INTEGER))
  AND (CAST(sqlc.narg(uploader) AS TEXT) IS NULL OR i.uploader_nickname = CAST(sqlc.narg(uploader) AS TEXT))
  AND (CAST(sqlc.narg(from_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) >= CAST(sqlc.narg(from_time) AS TEXT))
  AND (CAST(sqlc.narg(to_time) AS TEXT) IS NULL OR datetime(i.exif_time, printf('%+d seconds', COALESCE(i.time_offset_seconds, 0))) <= CAST(sqlc.narg(to_time) AS TEXT))
  AND (CAST(sqlc.narg(has_gps) AS INTEGER) IS NULL OR i.gps_lat IS NOT NULL)
  AND (CAST(sqlc.narg(tags) AS TEXT) IS NULL OR (
        SELECT COUNT(DISTINCT t.id) FROM image_tags it JOIN tags t ON t.id = it.tag_id
        WHERE it.image_id = i.id AND t.name IN (SELECT value FROM json_each(CAST(sqlc.narg(tags) AS TEXT)))
      ) >= sqlc.arg(tags_needed))
ORDER BY i.seq
LIMIT sqlc.arg(max_rows);

-- name: ListFolderUploaders :many
SELECT DISTINCT uploader_nickname FROM images
WHERE folder_id = ? AND deleted_at IS NULL
ORDER BY uploader_nickname;

-- name: SetImageGPS :exec
UPDATE images SET gps_lat = ?, gps_lon = ?, gps_attempted_at = ? WHERE id = ?;

-- name: MarkGPSAttempted :exec
UPDATE images SET gps_attempted_at = ? WHERE id = ?;

-- Trashed images are included: they may be restored.
-- name: ListImagesMissingGPS :many
SELECT i.id FROM images i
JOIN folders f ON f.id = i.folder_id
WHERE i.gps_attempted_at IS NULL AND f.deleted_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM jobs j
    WHERE j.type = 'extract_gps' AND j.status IN ('pending', 'running')
      AND json_extract(j.payload, '$.image_id') = i.id
  );
