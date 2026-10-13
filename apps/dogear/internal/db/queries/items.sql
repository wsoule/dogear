-- name: GetItem :one
SELECT * FROM items WHERE id = $1;

-- name: GetItemForUpdate :one
SELECT * FROM items WHERE id = $1 FOR UPDATE;

-- name: GetItemByNorm :one
SELECT * FROM items WHERE url_norm = $1;

-- name: InsertItem :one
INSERT INTO items (url, url_norm, domain, title, blurb, created_by, created_via_token, via, status, kind, creator, flags)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: SetItemStatus :exec
UPDATE items SET status = $2, updated_at = now(),
  removed_at = CASE WHEN $2 = 'removed' THEN now() ELSE removed_at END
WHERE id = $1;

-- name: SetItemFlags :exec
UPDATE items SET flags = $2, updated_at = now() WHERE id = $1;

-- name: SetItemCounts :exec
UPDATE items SET like_count = $2, dislike_count = $3, score = $4, updated_at = now() WHERE id = $1;

-- name: ItemCategory :one
SELECT c.id, c.slug, c.name, p.slug AS parent_slug
FROM item_categories ic
JOIN categories c ON c.id = ic.category_id
LEFT JOIN categories p ON p.id = c.parent_id
WHERE ic.item_id = $1
ORDER BY c.id LIMIT 1;

-- name: ItemCategories :many
SELECT DISTINCT ON (ic.item_id) ic.item_id, c.slug, c.name, p.slug AS parent_slug
FROM item_categories ic
JOIN categories c ON c.id = ic.category_id
LEFT JOIN categories p ON p.id = c.parent_id
WHERE ic.item_id = ANY(@ids::bigint[])
ORDER BY ic.item_id, c.id;

-- name: AddItemCategory :exec
INSERT INTO item_categories (item_id, category_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: ClearItemCategories :exec
DELETE FROM item_categories WHERE item_id = $1;

-- name: ItemSources :many
SELECT sl.repo, sl.title, s.heading_path, s.original_description
FROM item_sources s JOIN source_lists sl ON sl.id = s.source_list_id
WHERE s.item_id = $1 ORDER BY sl.stars DESC NULLS LAST, sl.repo;

-- name: DomainBlocked :one
SELECT EXISTS (SELECT 1 FROM blocked_domains WHERE domain = $1 OR $1 LIKE '%.' || domain);

-- name: BlockDomain :exec
INSERT INTO blocked_domains (domain, reason) VALUES ($1, $2) ON CONFLICT (domain) DO UPDATE SET reason = EXCLUDED.reason;

-- name: UserDomainPosts :one
-- Posts only: links the user imported (personal items) do not count.
SELECT count(*) FROM items
WHERE created_by = $1 AND domain = $2 AND created_at > now() - interval '7 days' AND status <> 'personal';

-- name: GetVote :one
SELECT * FROM votes WHERE user_id = $1 AND item_id = $2;

-- name: UpsertVote :exec
INSERT INTO votes (user_id, item_id, value, counted, via)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, item_id) DO UPDATE SET value = EXCLUDED.value, counted = EXCLUDED.counted,
  via = EXCLUDED.via, updated_at = now();

-- name: DeleteVote :exec
DELETE FROM votes WHERE user_id = $1 AND item_id = $2;

-- name: MyVotes :many
SELECT item_id, value FROM votes WHERE user_id = $1 AND item_id = ANY(@ids::bigint[]);

-- name: CountedVotesToday :one
SELECT count(*) FROM votes WHERE user_id = $1 AND counted AND updated_at > now() - interval '1 day';

-- name: CountedAgentVotesToday :one
SELECT count(*) FROM votes WHERE user_id = $1 AND counted AND via IN ('api','mcp')
  AND updated_at > now() - interval '1 day';

-- name: RecountItem :one
SELECT
  count(*) FILTER (WHERE value = 1 AND counted)::int AS likes,
  count(*) FILTER (WHERE value = -1 AND counted)::int AS dislikes
FROM votes WHERE item_id = $1;

-- name: ItemsVotedBy :many
SELECT item_id FROM votes WHERE user_id = $1;

-- name: SetUserVotesCounted :exec
UPDATE votes SET counted = $2 WHERE user_id = $1;

-- name: DeleteUserVotes :exec
DELETE FROM votes WHERE user_id = $1;

-- name: ItemsByUser :many
-- What the user posted; their imports (personal items) are in their
-- archive, not here.
SELECT * FROM items WHERE created_by = $1 AND status NOT IN ('removed','personal')
ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: AllItemsByUser :many
SELECT * FROM items WHERE created_by = $1 ORDER BY created_at DESC;

-- name: RemoveUserItems :exec
-- Removes what a user posted. Their imports that other people saved are
-- not posts and stay in those archives.
UPDATE items SET status = 'removed', removed_at = now(), updated_at = now() WHERE created_by = $1 AND status <> 'personal';

-- name: UserVotesExport :many
SELECT v.item_id, v.value, v.counted, v.via, v.created_at, i.url, i.title
FROM votes v JOIN items i ON i.id = v.item_id WHERE v.user_id = $1 ORDER BY v.created_at;

-- name: CreateReport :exec
INSERT INTO reports (item_id, reporter_id, reason, note) VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id, reporter_id) DO UPDATE SET reason = EXCLUDED.reason, note = EXCLUDED.note;

-- name: OpenReportCount :one
SELECT count(DISTINCT reporter_id) FROM reports WHERE item_id = $1 AND status = 'open';

-- name: OpenReports :many
SELECT r.id, r.item_id, r.reason, r.note, r.created_at, u.username AS reporter, i.title, i.url, i.status AS item_status
FROM reports r JOIN users u ON u.id = r.reporter_id JOIN items i ON i.id = r.item_id
WHERE r.status = 'open' ORDER BY r.created_at LIMIT 200;

-- name: SetReportStatus :one
UPDATE reports SET status = $2 WHERE id = $1 RETURNING *;

-- name: PendingItems :many
SELECT i.*, u.username AS poster
FROM items i LEFT JOIN users u ON u.id = i.created_by
WHERE i.status = 'pending' ORDER BY i.created_at LIMIT 200;

-- name: DeadItems :many
SELECT * FROM items WHERE link_status = 'dead' AND status <> 'removed'
ORDER BY link_checked_at DESC LIMIT 100;

-- name: InsertModAction :exec
INSERT INTO mod_actions (mod_id, item_id, user_id, action, note) VALUES ($1, $2, $3, $4, $5);

-- name: UpsertClassification :exec
INSERT INTO item_classifications (item_id, category_slug, category_conf, promo_conf, mismatch_conf,
  injection_conf, unsafe_conf, kind, kind_conf, model)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (item_id) DO UPDATE SET category_slug = EXCLUDED.category_slug,
  category_conf = EXCLUDED.category_conf, promo_conf = EXCLUDED.promo_conf,
  mismatch_conf = EXCLUDED.mismatch_conf, injection_conf = EXCLUDED.injection_conf,
  unsafe_conf = EXCLUDED.unsafe_conf, kind = EXCLUDED.kind, kind_conf = EXCLUDED.kind_conf,
  model = EXCLUDED.model, created_at = now();

-- name: GetClassification :one
SELECT * FROM item_classifications WHERE item_id = $1;

-- name: ItemsNeedingClassification :many
SELECT * FROM items WHERE status = 'pending' AND 'classifier_error' = ANY(flags)
ORDER BY created_at LIMIT 20;

-- name: ItemsToLinkCheck :many
SELECT id, url, link_fail_count FROM items
WHERE status <> 'removed'
ORDER BY link_checked_at NULLS FIRST LIMIT $1;

-- name: SetLinkResult :exec
UPDATE items SET link_status = $2, link_fail_count = $3, link_checked_at = now() WHERE id = $1;

-- name: RestoreLink :exec
UPDATE items SET link_status = 'unknown', link_fail_count = 0 WHERE id = $1;

-- name: SitemapItems :many
SELECT i.id, i.updated_at FROM items i
LEFT JOIN users u ON u.id = i.created_by
WHERE i.status = 'live' AND i.link_status <> 'dead'
  AND (i.via = 'import' OR (i.created_at < now() - interval '7 days' AND i.like_count >= 3
       AND u.trust_level >= 1 AND u.shadow_banned_at IS NULL))
ORDER BY i.score DESC LIMIT 45000;

-- name: GetThumb :one
SELECT mime, data FROM item_thumbs WHERE item_id = $1;

-- name: SaveThumb :exec
INSERT INTO item_thumbs (item_id, mime, data, width, height) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (item_id) DO UPDATE SET mime = EXCLUDED.mime, data = EXCLUDED.data,
  width = EXCLUDED.width, height = EXCLUDED.height, fetched_at = now();

-- name: SetImageState :exec
UPDATE items SET image_state = $2 WHERE id = $1;

-- name: SetImageURL :exec
UPDATE items SET image_url = $2, image_checked_at = now(),
  image_state = CASE WHEN $2::text = '' THEN 'none' ELSE image_state END
WHERE id = $1;

-- name: ItemsNeedingImage :many
-- Live items with no deterministic image source, best-ranked first.
SELECT id, url_norm FROM items
WHERE image_checked_at IS NULL AND status = 'live'
  AND domain NOT IN ('github.com', 'youtube.com')
ORDER BY score DESC
LIMIT $1;

-- name: GetTranscript :one
SELECT lang, text FROM item_transcripts WHERE item_id = $1 AND ok;

-- name: SaveTranscript :exec
INSERT INTO item_transcripts (item_id, lang, text, ok) VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id) DO UPDATE SET lang = EXCLUDED.lang, text = EXCLUDED.text, ok = EXCLUDED.ok, fetched_at = now();

-- name: VideosNeedingTranscript :many
SELECT i.id, i.url_norm FROM items i
WHERE i.status = 'live' AND i.url_norm LIKE 'https://youtube.com/watch?v=%'
  AND NOT EXISTS (SELECT 1 FROM item_transcripts t WHERE t.item_id = i.id)
ORDER BY i.score DESC
LIMIT $1;

-- name: ItemsNeedingEmbedding :many
SELECT i.id, i.title, i.blurb, i.domain, i.kind, i.creator,
  COALESCE((SELECT c.name FROM item_categories ic JOIN categories c ON c.id = ic.category_id WHERE ic.item_id = i.id ORDER BY c.id LIMIT 1), '')::text AS category,
  COALESCE((SELECT left(t.text, 600) FROM item_transcripts t WHERE t.item_id = i.id AND t.ok), '')::text AS transcript
FROM items i
WHERE i.embedding IS NULL AND i.status = 'live'
ORDER BY i.score DESC
LIMIT $1;

-- name: SetItemEmbedding :exec
UPDATE items SET embedding = $2, embedded_hash = $3 WHERE id = $1;

-- name: EmbeddingsAfter :many
-- Loads embeddings into the in-memory index in id order, in pages.
SELECT id, embedding FROM items
WHERE embedding IS NOT NULL AND status = 'live' AND id > $1
ORDER BY id
LIMIT $2;

-- name: ItemsByIDs :many
SELECT * FROM items WHERE id = ANY(@ids::bigint[]);

-- name: SetItemDetails :exec
-- Records what the media lookup found. The creator name fills in only when
-- the poster left it blank; media_checked_at is set even when nothing was
-- found so the backfill moves on. A personal item's title came from one
-- importer's file, so the page's own title replaces it, as it does a title
-- made up from the address (flag 'untitled').
UPDATE items SET
  title = CASE WHEN ('untitled' = ANY(flags) OR status = 'personal') AND @title::text <> '' THEN @title::text ELSE title END,
  flags = CASE WHEN 'untitled' = ANY(flags) AND @title::text <> '' THEN array_remove(flags, 'untitled') ELSE flags END,
  creator_id = COALESCE(sqlc.narg(creator_id)::bigint, creator_id),
  creator = CASE WHEN creator = '' THEN @creator::text ELSE creator END,
  duration_seconds = COALESCE(sqlc.narg(duration_seconds)::int, duration_seconds),
  published_at = COALESCE(sqlc.narg(published_at)::timestamptz, published_at),
  reading_minutes = COALESCE(sqlc.narg(reading_minutes)::smallint, reading_minutes),
  site_name = CASE WHEN @site_name::text <> '' THEN @site_name::text ELSE site_name END,
  media_checked_at = now(),
  media_ok = @ok::bool
WHERE id = @id;

-- name: ItemsNeedingMedia :many
-- Videos, articles, and newsletter posts not yet looked at, plus every
-- personal (imported) item, whose title should come from the page itself; live items first, then people's
-- own posts, then by rank. trusted is true for imports and for live posts
-- by trusted, unflagged users: only their works may add new creator pages.
-- Personal items (unreviewed imports) are never trusted. A missing poster
-- alone is not trust: created_by also goes NULL when an account is deleted.
SELECT i.id, i.url_norm, i.kind,
  (i.status = 'live' AND (i.via = 'import' OR EXISTS (SELECT 1 FROM users u WHERE u.id = i.created_by
     AND u.trust_level >= 1 AND u.shadow_banned_at IS NULL AND u.banned_at IS NULL)))::bool AS trusted
FROM items i
WHERE i.status IN ('live','personal') AND i.via <> 'auto'
  AND (i.media_checked_at IS NULL OR (NOT i.media_ok AND i.media_checked_at < now() - interval '7 days'))
  AND (i.kind IN ('video','article') OR i.domain LIKE '%.substack.com' OR i.domain = 'medium.com'
       OR i.status = 'personal')
ORDER BY (i.media_checked_at IS NULL) DESC, (i.status = 'live') DESC, (i.via IN ('web','api','mcp')) DESC, i.score DESC
LIMIT $1;

-- name: CreatorWorks :many
-- A creator's works on Dogear, best first.
SELECT * FROM items
WHERE creator_id = $1 AND status = 'live' AND link_status <> 'dead'
ORDER BY score DESC, id DESC
LIMIT $2;

-- name: CreatorsByName :many
-- Live channels and people whose name or address contains q, best first.
SELECT * FROM items
WHERE kind IN ('channel','person') AND status = 'live' AND link_status <> 'dead'
  AND (title ILIKE '%' || @q::text || '%' OR url_norm ILIKE '%' || @q::text || '%')
ORDER BY score DESC, id DESC
LIMIT @lim;
