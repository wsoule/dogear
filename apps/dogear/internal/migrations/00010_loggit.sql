-- +goose Up
-- loggit integration: a connected loggit account per user (OAuth tokens,
-- encrypted), 1:1 links between Dogear lists and loggit lists, and a cache
-- mapping Dogear items to loggit media.
CREATE TABLE loggit_accounts (
  user_id           BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  loggit_user_id    TEXT NOT NULL,
  loggit_username   TEXT NOT NULL,
  access_token_enc  BYTEA NOT NULL,
  refresh_token_enc BYTEA NOT NULL,
  access_expires_at TIMESTAMPTZ NOT NULL,
  scope             TEXT NOT NULL DEFAULT '',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE list_links (
  list_id         BIGINT PRIMARY KEY REFERENCES lists(id) ON DELETE CASCADE,
  user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  loggit_list_id  TEXT NOT NULL,
  -- What both sides looked like after the last successful sync; the
  -- three-way merge compares each side against it.
  baseline        JSONB NOT NULL DEFAULT '{}',
  status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','broken')),
  last_error      TEXT NOT NULL DEFAULT '',
  dirty_at        TIMESTAMPTZ,
  last_synced_at  TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, loggit_list_id)
);

-- media_id NULL means loggit cannot represent the item (a generic link).
CREATE TABLE item_media (
  item_id     BIGINT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  media_id    TEXT,
  resolved_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX item_media_media ON item_media (media_id) WHERE media_id IS NOT NULL;

-- Entry edit times, so "most recent edit wins" can compare both sides.
ALTER TABLE list_items ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
UPDATE list_items SET updated_at = added_at;

-- Items created from loggit media land in their own topic categories.
INSERT INTO categories (slug, name, sort_order) VALUES ('film-books-games', 'Film, Books & Games', 950)
ON CONFLICT (slug) DO NOTHING;
INSERT INTO categories (slug, name, parent_id, sort_order)
SELECT v.slug, v.name, p.id, v.ord
FROM (VALUES ('film-tv', 'Film & TV', 1), ('books-shelf', 'Books', 2), ('games-shelf', 'Games', 3)) AS v(slug, name, ord)
JOIN categories p ON p.slug = 'film-books-games'
ON CONFLICT (slug) DO NOTHING;

-- +goose Down
ALTER TABLE list_items DROP COLUMN updated_at;
DROP TABLE item_media, list_links, loggit_accounts;
