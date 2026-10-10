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
SELECT count(*) FROM items
WHERE created_by = $1 AND domain = $2 AND created_at > now() - interval '7 days';

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
SELECT * FROM items WHERE created_by = $1 AND status <> 'removed'
ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: AllItemsByUser :many
SELECT * FROM items WHERE created_by = $1 ORDER BY created_at DESC;

-- name: RemoveUserItems :exec
UPDATE items SET status = 'removed', removed_at = now(), updated_at = now() WHERE created_by = $1;

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
