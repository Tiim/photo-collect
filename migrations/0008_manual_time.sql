-- +goose Up
-- Capture time set by hand (naive wall-clock time, same format as exif_time).
-- It overrides the EXIF time plus the calibration offset, and is never touched
-- by RecomputeDeviceOffsets.
ALTER TABLE images ADD COLUMN manual_time TEXT;

-- +goose Down
ALTER TABLE images DROP COLUMN manual_time;
