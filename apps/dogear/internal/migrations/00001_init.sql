-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE users (
  id                BIGSERIAL PRIMARY KEY,
  signup_ip_hash    BYTEA,
  username          CITEXT NOT NULL UNIQUE,
  avatar_url        TEXT,
  shadow_banned_at  TIMESTAMPTZ,
  role              TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','mod','admin')),
  trust_level       SMALLINT NOT NULL DEFAULT 0,
  approved_posts    INT NOT NULL DEFAULT 0,
  banned_at         TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
  id_hash    BYTEA PRIMARY KEY,
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id           BIGSERIAL PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  token_hash   BYTEA NOT NULL UNIQUE,
  scopes       TEXT[] NOT NULL DEFAULT '{read,post,vote}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);

CREATE TABLE categories (
  id         SERIAL PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  parent_id  INT REFERENCES categories(id),
  sort_order INT NOT NULL DEFAULT 0
);

CREATE TABLE items (
  id                BIGSERIAL PRIMARY KEY,
  url               TEXT NOT NULL,
  url_norm          TEXT NOT NULL UNIQUE,
  domain            TEXT NOT NULL,
  title             TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
  blurb             TEXT NOT NULL DEFAULT '' CHECK (char_length(blurb) <= 280),
  created_by        BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_via_token BIGINT REFERENCES api_tokens(id) ON DELETE SET NULL,
  via               TEXT NOT NULL DEFAULT 'web' CHECK (via IN ('import','web','api','mcp')),
  status            TEXT NOT NULL DEFAULT 'live' CHECK (status IN ('live','pending','removed')),
  link_status       TEXT NOT NULL DEFAULT 'unknown' CHECK (link_status IN ('unknown','ok','dead')),
  link_fail_count   SMALLINT NOT NULL DEFAULT 0,
  link_checked_at   TIMESTAMPTZ,
  github_stars      INT,
  list_count        INT NOT NULL DEFAULT 0,
  prior_score       REAL NOT NULL DEFAULT 0,
  like_count        INT NOT NULL DEFAULT 0,
  dislike_count     INT NOT NULL DEFAULT 0,
  score             DOUBLE PRECISION NOT NULL DEFAULT 0,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX items_top  ON items (score DESC, id DESC) WHERE status = 'live';
CREATE INDEX items_new  ON items (created_at DESC, id DESC) WHERE status = 'live' AND via <> 'import';
CREATE INDEX items_text ON items USING gin ((title || ' ' || blurb || ' ' || domain) gin_trgm_ops);
CREATE INDEX items_linkcheck ON items (link_checked_at NULLS FIRST) WHERE status <> 'removed';

CREATE TABLE item_categories (
  item_id     BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  category_id INT NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, category_id)
);
CREATE INDEX item_categories_cat ON item_categories (category_id, item_id);

CREATE TABLE source_lists (
  id               SERIAL PRIMARY KEY,
  repo             TEXT NOT NULL UNIQUE,
  title            TEXT NOT NULL,
  category_id      INT REFERENCES categories(id),
  license          TEXT,
  stars            INT,
  readme_sha       TEXT,
  last_imported_at TIMESTAMPTZ
);

CREATE TABLE item_sources (
  item_id              BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  source_list_id       INT NOT NULL REFERENCES source_lists(id) ON DELETE CASCADE,
  heading_path         TEXT NOT NULL DEFAULT '',
  original_description TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (item_id, source_list_id)
);

CREATE TABLE votes (
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  value      SMALLINT NOT NULL CHECK (value IN (-1, 1)),
  counted    BOOLEAN NOT NULL DEFAULT true,
  via        TEXT NOT NULL DEFAULT 'web',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, item_id)
);
CREATE INDEX votes_item ON votes (item_id);

CREATE TABLE reports (
  id          BIGSERIAL PRIMARY KEY,
  item_id     BIGINT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  reporter_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  reason      TEXT NOT NULL CHECK (reason IN ('spam','dead','offensive','duplicate','other')),
  note        TEXT NOT NULL DEFAULT '' CHECK (char_length(note) <= 500),
  status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','upheld','dismissed')),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (item_id, reporter_id)
);

CREATE TABLE mod_actions (
  id         BIGSERIAL PRIMARY KEY,
  mod_id     BIGINT NOT NULL REFERENCES users(id),
  item_id    BIGINT REFERENCES items(id) ON DELETE SET NULL,
  user_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
  action     TEXT NOT NULL,
  note       TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE blocked_domains (
  domain     TEXT PRIMARY KEY,
  reason     TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE blocked_domains, mod_actions, reports, votes, item_sources, source_lists,
  item_categories, items, categories, api_tokens, sessions, users;
