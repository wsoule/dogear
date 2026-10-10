-- +goose Up
-- Video transcripts, fetched server-side for YouTube items. ok = false
-- records that none was available so the job does not retry forever.
CREATE TABLE item_transcripts (
  item_id    BIGINT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  lang       TEXT NOT NULL DEFAULT '',
  text       TEXT NOT NULL DEFAULT '',
  ok         BOOLEAN NOT NULL,
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX item_transcripts_text ON item_transcripts USING gin (text gin_trgm_ops) WHERE ok;

-- +goose Down
DROP TABLE item_transcripts;
