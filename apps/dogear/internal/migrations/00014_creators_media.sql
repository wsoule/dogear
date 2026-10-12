-- +goose Up
-- Creators are items too: a YouTube channel or a publication is kind
-- 'channel', a person's profile is kind 'person', so they can be liked,
-- dogeared, tagged, and searched like anything else. Works point at their
-- creator. via 'auto' marks creators Dogear added from a work's metadata.
ALTER TABLE items DROP CONSTRAINT items_kind_check;
ALTER TABLE items ADD CONSTRAINT items_kind_check
  CHECK (kind IN ('site','tool','app','book','device','video','article','channel','person','other'));
ALTER TABLE items DROP CONSTRAINT items_via_check;
ALTER TABLE items ADD CONSTRAINT items_via_check CHECK (via IN ('import','web','api','mcp','auto'));

-- What a link is beyond its title: a video's length and date, an article's
-- site and reading time. media_checked_at marks items the backfill has
-- looked at, whether or not it found anything.
ALTER TABLE items
  ADD COLUMN creator_id       BIGINT REFERENCES items(id) ON DELETE SET NULL,
  ADD COLUMN duration_seconds INT,
  ADD COLUMN published_at     TIMESTAMPTZ,
  ADD COLUMN reading_minutes  SMALLINT,
  ADD COLUMN site_name        TEXT NOT NULL DEFAULT '' CHECK (char_length(site_name) <= 80),
  ADD COLUMN media_checked_at TIMESTAMPTZ;
CREATE INDEX items_creator_works ON items (creator_id, score DESC, id DESC) WHERE status = 'live' AND creator_id IS NOT NULL;
CREATE INDEX items_media_todo ON items (score DESC) WHERE status = 'live' AND media_checked_at IS NULL;

-- YouTube channel pages were filed as videos; they are channels.
UPDATE items SET kind = 'channel'
WHERE kind = 'video' AND url_norm ~ '^https://youtube\.com/(@[^/?]+|channel/[^/?]+|c/[^/?]+|user/[^/?]+)$';

-- +goose Down
UPDATE items SET kind = 'video' WHERE kind = 'channel' AND domain = 'youtube.com';
UPDATE items SET kind = 'site' WHERE kind IN ('channel','person');
DELETE FROM items WHERE via = 'auto';
DROP INDEX items_media_todo;
DROP INDEX items_creator_works;
ALTER TABLE items DROP COLUMN creator_id, DROP COLUMN duration_seconds, DROP COLUMN published_at,
  DROP COLUMN reading_minutes, DROP COLUMN site_name, DROP COLUMN media_checked_at;
ALTER TABLE items DROP CONSTRAINT items_via_check;
ALTER TABLE items ADD CONSTRAINT items_via_check CHECK (via IN ('import','web','api','mcp'));
ALTER TABLE items DROP CONSTRAINT items_kind_check;
ALTER TABLE items ADD CONSTRAINT items_kind_check
  CHECK (kind IN ('site','tool','app','book','device','video','article','other'));
