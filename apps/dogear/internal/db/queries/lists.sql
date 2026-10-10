-- name: CreateList :one
INSERT INTO lists (user_id, slug, title, description, visibility, sort_order)
VALUES ($1, $2, $3, $4, $5, COALESCE((SELECT max(sort_order) + 1 FROM lists WHERE user_id = $1), 0))
RETURNING *;

-- name: GetList :one
SELECT * FROM lists WHERE user_id = $1 AND slug = $2;

-- name: UserLists :many
SELECT l.*, (SELECT count(*) FROM list_items li WHERE li.list_id = l.id)::int AS entry_count
FROM lists l WHERE l.user_id = $1 ORDER BY l.sort_order, l.id;

-- name: CountUserLists :one
SELECT count(*) FROM lists WHERE user_id = $1;

-- name: ListSlugTaken :one
SELECT EXISTS (SELECT 1 FROM lists WHERE user_id = $1 AND slug = $2);

-- name: UpdateList :one
UPDATE lists SET title = $3, description = $4, visibility = $5, slug = $6, updated_at = now()
WHERE user_id = $1 AND id = $2 RETURNING *;

-- name: SetListOrder :exec
UPDATE lists SET sort_order = $3 WHERE user_id = $1 AND id = $2;

-- name: DeleteList :exec
DELETE FROM lists WHERE user_id = $1 AND id = $2;

-- name: TouchList :exec
UPDATE lists SET updated_at = now() WHERE id = $1;

-- name: CountListEntries :one
SELECT count(*) FROM list_items WHERE list_id = $1;

-- name: GetListEntry :one
SELECT * FROM list_items WHERE list_id = $1 AND item_id = $2;

-- name: UpsertListEntry :exec
INSERT INTO list_items (list_id, item_id, note, position)
VALUES ($1, $2, $3, COALESCE(sqlc.narg(position)::int, (SELECT COALESCE(max(position) + 1, 0) FROM list_items WHERE list_id = $1)))
ON CONFLICT (list_id, item_id) DO UPDATE SET note = EXCLUDED.note,
  position = COALESCE(sqlc.narg(position)::int, list_items.position),
  updated_at = CASE WHEN list_items.note IS DISTINCT FROM EXCLUDED.note
    OR list_items.position IS DISTINCT FROM COALESCE(sqlc.narg(position)::int, list_items.position)
    THEN now() ELSE list_items.updated_at END;

-- name: DeleteListEntry :exec
DELETE FROM list_items WHERE list_id = $1 AND item_id = $2;

-- name: SetEntryPosition :exec
UPDATE list_items SET position = $3, updated_at = CASE WHEN position <> $3 THEN now() ELSE updated_at END
WHERE list_id = $1 AND item_id = $2;

-- name: ListEntries :many
SELECT li.note, li.position, li.added_at, i.*
FROM list_items li JOIN items i ON i.id = li.item_id
WHERE li.list_id = $1
ORDER BY li.position, li.added_at;

-- name: ItemSavedIn :many
SELECT l.slug FROM list_items li JOIN lists l ON l.id = li.list_id
WHERE l.user_id = $1 AND li.item_id = $2;

-- name: SyncRow :one
SELECT * FROM github_sync WHERE user_id = $1;

-- name: UpsertSyncRow :exec
INSERT INTO github_sync (user_id, installation_id, repo_full_name, dirty_since, status)
VALUES ($1, $2, $3, now() - interval '10 minutes', 'active')
ON CONFLICT (user_id) DO UPDATE SET installation_id = EXCLUDED.installation_id,
  repo_full_name = EXCLUDED.repo_full_name, status = 'active', content_sha256 = NULL,
  dirty_since = now() - interval '10 minutes';

-- name: DeleteSyncRow :exec
DELETE FROM github_sync WHERE user_id = $1;

-- name: SetIncludePosted :exec
UPDATE github_sync SET include_posted = $2, dirty_since = COALESCE(dirty_since, now()) WHERE user_id = $1;

-- name: MarkDirty :exec
UPDATE github_sync SET dirty_since = COALESCE(dirty_since, now()) WHERE user_id = $1 AND status = 'active';

-- name: MarkDirtyForItem :exec
UPDATE github_sync g SET dirty_since = COALESCE(g.dirty_since, now())
WHERE g.status = 'active' AND (
  g.user_id IN (SELECT l.user_id FROM list_items li JOIN lists l ON l.id = li.list_id
                WHERE li.item_id = $1 AND l.visibility = 'public')
  OR (g.include_posted AND g.user_id = (SELECT created_by FROM items WHERE id = $1)));

-- name: DirtySyncRows :many
SELECT * FROM github_sync
WHERE status = 'active' AND dirty_since < $1
  AND (last_synced_at IS NULL OR last_synced_at < now() - interval '5 minutes');

-- name: MarkSynced :exec
UPDATE github_sync SET content_sha256 = $2, last_synced_at = now(), dirty_since = NULL WHERE user_id = $1;

-- name: SetSyncStatus :exec
UPDATE github_sync SET status = $2 WHERE user_id = $1;

-- name: RevokeSyncByInstallation :exec
UPDATE github_sync SET status = 'revoked' WHERE installation_id = $1;

-- name: SyncRowByInstallation :many
SELECT * FROM github_sync WHERE installation_id = $1;

-- name: PublicLists :many
-- Recently updated public lists with at least one entry, for /lists.
SELECT l.slug, l.title, l.description, l.updated_at, u.username,
  (SELECT count(*) FROM list_items li WHERE li.list_id = l.id)::int AS entry_count
FROM lists l JOIN users u ON u.id = l.user_id
WHERE l.visibility = 'public' AND u.banned_at IS NULL AND u.shadow_banned_at IS NULL
  AND EXISTS (SELECT 1 FROM list_items li WHERE li.list_id = l.id)
ORDER BY l.updated_at DESC
LIMIT 100;

-- name: GetLoggitAccount :one
SELECT * FROM loggit_accounts WHERE user_id = $1;

-- name: UpsertLoggitAccount :exec
INSERT INTO loggit_accounts (user_id, loggit_user_id, loggit_username, access_token_enc, refresh_token_enc, access_expires_at, scope)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id) DO UPDATE SET loggit_user_id = EXCLUDED.loggit_user_id,
  loggit_username = EXCLUDED.loggit_username, access_token_enc = EXCLUDED.access_token_enc,
  refresh_token_enc = EXCLUDED.refresh_token_enc, access_expires_at = EXCLUDED.access_expires_at,
  scope = EXCLUDED.scope;

-- name: DeleteLoggitAccount :exec
DELETE FROM loggit_accounts WHERE user_id = $1;

-- name: GetListLink :one
SELECT * FROM list_links WHERE list_id = $1;

-- name: CreateListLink :exec
INSERT INTO list_links (list_id, user_id, loggit_list_id, dirty_at) VALUES ($1, $2, $3, now());

-- name: DeleteListLink :exec
DELETE FROM list_links WHERE list_id = $1;

-- name: DeleteUserListLinks :exec
DELETE FROM list_links WHERE user_id = $1;

-- name: MarkListLinkDirty :exec
UPDATE list_links SET dirty_at = COALESCE(dirty_at, now()) WHERE list_id = $1 AND status = 'active';

-- name: ListLinksToSync :many
-- Links edited locally, or not synced for two minutes.
SELECT * FROM list_links
WHERE status = 'active' AND (dirty_at IS NOT NULL OR last_synced_at IS NULL OR last_synced_at < now() - interval '2 minutes')
ORDER BY dirty_at NULLS LAST, last_synced_at NULLS FIRST
LIMIT $1;

-- name: SaveListLinkSync :exec
UPDATE list_links SET baseline = $2, last_synced_at = now(), dirty_at = NULL, last_error = '' WHERE list_id = $1;

-- name: SetListLinkError :exec
UPDATE list_links SET last_error = $2, status = $3, last_synced_at = now(), dirty_at = NULL WHERE list_id = $1;

-- name: LinkedLoggitListIDs :many
SELECT loggit_list_id FROM list_links WHERE user_id = $1;

-- name: GetItemMedia :many
SELECT item_id, media_id, resolved_at FROM item_media WHERE item_id = ANY(@ids::bigint[]);

-- name: SetItemMedia :exec
INSERT INTO item_media (item_id, media_id) VALUES ($1, $2)
ON CONFLICT (item_id) DO UPDATE SET media_id = EXCLUDED.media_id, resolved_at = now();

-- name: ItemForMedia :one
SELECT item_id FROM item_media WHERE media_id = $1 LIMIT 1;

-- name: ListEntryTimes :many
SELECT item_id, note, position, updated_at FROM list_items WHERE list_id = $1 ORDER BY position, added_at;

-- name: GetListByID :one
SELECT * FROM lists WHERE id = $1;

-- name: SetListFields :exec
UPDATE lists SET title = $2, description = $3, visibility = $4, updated_at = now() WHERE id = $1;

-- name: InsertListEntryAtEnd :exec
INSERT INTO list_items (list_id, item_id, note, position)
VALUES ($1, $2, $3, (SELECT COALESCE(max(position) + 1, 0) FROM list_items WHERE list_id = $1))
ON CONFLICT (list_id, item_id) DO NOTHING;

-- name: SetEntryNote :exec
UPDATE list_items SET note = $3, updated_at = now() WHERE list_id = $1 AND item_id = $2;

-- name: DogearedAmong :many
-- Which of these items the user has in their Dogeared list.
SELECT li.item_id FROM list_items li JOIN lists l ON l.id = li.list_id
WHERE l.user_id = $1 AND l.slug = 'dogeared' AND li.item_id = ANY(@ids::bigint[]);
