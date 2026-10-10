-- +goose Up
-- Preview images. image_url is the discovered source (og:image) for sites
-- without a deterministic one; image_state records whether an image exists.
ALTER TABLE items
  ADD COLUMN image_url TEXT,
  ADD COLUMN image_state TEXT NOT NULL DEFAULT 'unknown' CHECK (image_state IN ('unknown','ok','none')),
  ADD COLUMN image_checked_at TIMESTAMPTZ;
CREATE INDEX items_image_backfill ON items (score DESC) WHERE image_checked_at IS NULL AND status = 'live';

-- Small re-encoded thumbnails served from /img/{id}, so visitors never load
-- third-party images.
CREATE TABLE item_thumbs (
  item_id    BIGINT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  mime       TEXT NOT NULL,
  data       BYTEA NOT NULL,
  width      INT NOT NULL,
  height     INT NOT NULL,
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE item_thumbs;
DROP INDEX items_image_backfill;
ALTER TABLE items DROP COLUMN image_url, DROP COLUMN image_state, DROP COLUMN image_checked_at;
