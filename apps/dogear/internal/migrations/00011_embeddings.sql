-- +goose Up
-- Semantic search: a 256-dimension int8 Voyage embedding per item (256
-- bytes), searched in memory by the app. embedded_text_hash lets the job
-- re-embed an item when its title, reason, or category change.
ALTER TABLE items ADD COLUMN embedding BYTEA, ADD COLUMN embedded_hash BYTEA;
CREATE INDEX items_needs_embedding ON items (score DESC) WHERE embedding IS NULL AND status = 'live';

-- +goose Down
DROP INDEX items_needs_embedding;
ALTER TABLE items DROP COLUMN embedding, DROP COLUMN embedded_hash;
