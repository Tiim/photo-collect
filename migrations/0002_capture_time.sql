-- +goose Up
-- exif_time and calib_ref_time are naive wall-clock times (YYYY-MM-DDTHH:MM:SS,
-- no zone): EXIF carries no reliable zone, and offsets are computed on wall time.
ALTER TABLE images ADD COLUMN device_key          TEXT;
ALTER TABLE images ADD COLUMN exif_time           TEXT;
ALTER TABLE images ADD COLUMN qr_scanned          INTEGER NOT NULL DEFAULT 0;
ALTER TABLE images ADD COLUMN is_calibration      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE images ADD COLUMN calib_ref_time      TEXT;
ALTER TABLE images ADD COLUMN time_offset_seconds INTEGER;
CREATE INDEX images_device ON images(folder_id, device_key);

-- +goose Down
DROP INDEX images_device;
ALTER TABLE images DROP COLUMN time_offset_seconds;
ALTER TABLE images DROP COLUMN calib_ref_time;
ALTER TABLE images DROP COLUMN is_calibration;
ALTER TABLE images DROP COLUMN qr_scanned;
ALTER TABLE images DROP COLUMN exif_time;
ALTER TABLE images DROP COLUMN device_key;
