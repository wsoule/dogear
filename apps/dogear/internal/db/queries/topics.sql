-- name: ListTopics :many
-- Every topic with how many live items it holds.
SELECT t.id, t.slug, t.name, t.description, t.sort_order,
  (SELECT count(*) FROM item_topics it JOIN items i ON i.id = it.item_id
   WHERE it.topic_id = t.id AND i.status = 'live' AND i.link_status <> 'dead')::int AS item_count
FROM topics t ORDER BY t.sort_order, t.id;

-- name: TopicPatterns :many
SELECT id, slug, description, pattern FROM topics ORDER BY sort_order, id;

-- name: GetTopic :one
SELECT * FROM topics WHERE slug = $1;

-- name: FileTopicsByKeywordBatch :execrows
-- Files the next batch of not-yet-filed live items into every topic whose
-- pattern matches their title, reason, or creator, then marks the batch
-- filed. Returns how many items it marked; 0 means nothing is left.
WITH batch AS (
  SELECT id FROM items WHERE status = 'live' AND topics_filed_at IS NULL ORDER BY id LIMIT @lim
), filed AS (
  INSERT INTO item_topics (item_id, topic_id, source)
  SELECT i.id, t.id, 'keyword'
  FROM batch b JOIN items i ON i.id = b.id
  JOIN topics t ON (i.title || ' ' || i.blurb || ' ' || i.creator) ~* t.pattern
  ON CONFLICT DO NOTHING
)
UPDATE items SET topics_filed_at = now() WHERE id IN (SELECT id FROM batch);

-- name: FileItemTopicsByKeyword :exec
-- Files one item into every topic whose pattern matches it, right after it
-- is posted, so it doesn't wait for the hourly pass.
INSERT INTO item_topics (item_id, topic_id, source)
SELECT i.id, t.id, 'keyword'
FROM items i JOIN topics t ON (i.title || ' ' || i.blurb || ' ' || i.creator) ~* t.pattern
WHERE i.id = @id AND i.status = 'live'
ON CONFLICT DO NOTHING;

-- name: FileTopicByMeaning :execrows
INSERT INTO item_topics (item_id, topic_id, source)
SELECT x, @topic_id::int, 'meaning' FROM unnest(@ids::bigint[]) AS x
WHERE EXISTS (SELECT 1 FROM items i WHERE i.id = x AND i.status = 'live')
ON CONFLICT DO NOTHING;

-- name: TopicsForItems :many
SELECT it.item_id, t.slug, t.name FROM item_topics it JOIN topics t ON t.id = it.topic_id
WHERE it.item_id = ANY(@ids::bigint[]) ORDER BY t.sort_order;

-- name: TopicsMatching :many
-- The topics whose keyword pattern matches some text, in display order.
SELECT slug, name, description FROM topics WHERE @text::text ~* pattern ORDER BY sort_order;

-- name: AllTopics :many
SELECT slug, name, description FROM topics ORDER BY sort_order;
