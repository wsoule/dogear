-- +goose Up
ALTER TABLE users
  ADD COLUMN display_name TEXT CHECK (char_length(display_name) <= 60),
  ADD COLUMN bio          TEXT NOT NULL DEFAULT '' CHECK (char_length(bio) <= 280),
  ADD COLUMN website      TEXT;

ALTER TABLE items
  ADD COLUMN kind    TEXT NOT NULL DEFAULT 'site'
    CHECK (kind IN ('site','tool','app','book','device','video','article','other')),
  ADD COLUMN creator TEXT NOT NULL DEFAULT '' CHECK (char_length(creator) <= 120);
CREATE INDEX items_kind_top ON items (kind, score DESC, id DESC) WHERE status = 'live';

ALTER TABLE item_classifications ADD COLUMN kind TEXT, ADD COLUMN kind_conf REAL;

CREATE TABLE lists (
  id          BIGSERIAL PRIMARY KEY,
  user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  slug        TEXT NOT NULL,
  title       TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 80),
  description TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 280),
  visibility  TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','unlisted','private')),
  sort_order  INT NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, slug)
);

CREATE TABLE list_items (
  list_id  BIGINT NOT NULL REFERENCES lists(id) ON DELETE CASCADE,
  item_id  BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  note     TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 280),
  position INT NOT NULL DEFAULT 0,
  added_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (list_id, item_id)
);
CREATE INDEX list_items_item ON list_items (item_id);

-- +goose Down
DROP TABLE list_items, lists;
ALTER TABLE item_classifications DROP COLUMN kind, DROP COLUMN kind_conf;
DROP INDEX items_kind_top;
ALTER TABLE items DROP COLUMN kind, DROP COLUMN creator;
ALTER TABLE users DROP COLUMN display_name, DROP COLUMN bio, DROP COLUMN website;
