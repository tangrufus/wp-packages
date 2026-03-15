-- +goose Up
ALTER TABLE builds ADD COLUMN r2_upload_seconds INTEGER;

-- +goose Down
ALTER TABLE builds DROP COLUMN r2_upload_seconds;
