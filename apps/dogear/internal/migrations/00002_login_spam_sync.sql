-- +goose Up
ALTER TABLE users ADD COLUMN invited_by BIGINT REFERENCES users(id);

CREATE TABLE identities (
  id                  BIGSERIAL PRIMARY KEY,
  user_id             BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider            TEXT NOT NULL CHECK (provider IN ('github','google','apple')),
  provider_user_id    TEXT NOT NULL,
  provider_created_at TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_user_id)
);
CREATE INDEX identities_user ON identities (user_id);

CREATE TABLE invites (
  code_hash  BYTEA PRIMARY KEY,
  inviter_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  used_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  used_at    TIMESTAMPTZ
);

CREATE TABLE item_classifications (
  item_id        BIGINT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  category_slug  TEXT,
  category_conf  REAL,
  promo_conf     REAL,
  mismatch_conf  REAL,
  injection_conf REAL,
  unsafe_conf    REAL,
  model          TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE github_sync (
  user_id         BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  installation_id BIGINT NOT NULL,
  repo_full_name  TEXT NOT NULL,
  include_posted  BOOLEAN NOT NULL DEFAULT false,
  content_sha256  BYTEA,
  last_synced_at  TIMESTAMPTZ,
  dirty_since     TIMESTAMPTZ,
  status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','revoked'))
);

-- +goose Down
DROP TABLE github_sync, item_classifications, invites, identities;
ALTER TABLE users DROP COLUMN invited_by;
