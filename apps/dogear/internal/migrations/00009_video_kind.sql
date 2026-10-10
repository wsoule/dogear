-- +goose Up
-- Imported YouTube links were stored as "site"; they are videos.
UPDATE items SET kind = 'video'
WHERE via = 'import' AND kind = 'site' AND url_norm LIKE 'https://youtube.com/watch?v=%';

-- +goose Down
SELECT 1;
