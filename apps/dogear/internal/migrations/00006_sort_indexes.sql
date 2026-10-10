-- +goose Up
-- Keyset indexes for the "stars" and "a-z" feed sorts. A–Z ignores leading
-- punctuation and compares bytewise so Latin titles come first in plain order.
CREATE INDEX items_stars ON items ((COALESCE(github_stars, -1)) DESC, id DESC) WHERE status = 'live';
CREATE INDEX items_az ON items ((regexp_replace(lower(title), '^[^[:alnum:]]+', '') COLLATE "C"), id) WHERE status = 'live';

-- +goose Down
DROP INDEX items_az;
DROP INDEX items_stars;
