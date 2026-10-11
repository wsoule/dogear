-- +goose Up
-- Embeddings moved from Voyage to OpenAI; vectors from different models are
-- not comparable, so drop any old ones and let the backfill re-embed.
UPDATE items SET embedding = NULL, embedded_hash = NULL WHERE embedding IS NOT NULL;

-- +goose Down
SELECT 1;
