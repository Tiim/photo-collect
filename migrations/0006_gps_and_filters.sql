-- +goose Up
-- GPS position from the EXIF GPS IFD, decimal degrees. Both set or both NULL.
ALTER TABLE images ADD COLUMN gps_lat REAL;
ALTER TABLE images ADD COLUMN gps_lon REAL CHECK ((gps_lat IS NULL) = (gps_lon IS NULL));
-- Set once the extract_gps job has looked at an image (found or not), so the
-- startup backfill runs once per image.
ALTER TABLE images ADD COLUMN gps_attempted_at TEXT;

CREATE INDEX images_folder_rating ON images(folder_id, rating);
CREATE INDEX image_tags_tag_image ON image_tags(tag_id, image_id);

-- +goose Down
DROP INDEX image_tags_tag_image;
DROP INDEX images_folder_rating;
ALTER TABLE images DROP COLUMN gps_attempted_at;
ALTER TABLE images DROP COLUMN gps_lon;
ALTER TABLE images DROP COLUMN gps_lat;
