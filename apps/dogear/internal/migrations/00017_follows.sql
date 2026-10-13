-- +goose Up
-- Who follows whom. Following someone puts their public saves on your
-- /following page; it is one-way and needs no approval, like a public RSS
-- subscription to their archive.
CREATE TABLE follows (
  follower_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  followee_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (follower_id, followee_id),
  CHECK (follower_id <> followee_id)
);
CREATE INDEX follows_followee ON follows (followee_id);

-- The home page's "recently saved" strip reads the newest saves across
-- every user.
CREATE INDEX saves_saved_at ON saves (saved_at DESC);

-- +goose Down
DROP INDEX saves_saved_at;
DROP TABLE follows;
