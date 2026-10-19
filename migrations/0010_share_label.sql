-- +goose Up
-- Private name of a share link, e.g. its recipient. Only signed-in users see
-- it; visitors see the title (shares.name).
ALTER TABLE shares ADD COLUMN label TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE shares DROP COLUMN label;
