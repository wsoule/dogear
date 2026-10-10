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
  position = COALESCE(sqlc.narg(position)::int, list_items.position);

-- name: DeleteListEntry :exec
DELETE FROM list_items WHERE list_id = $1 AND item_id = $2;

-- name: SetEntryPosition :exec
UPDATE list_items SET position = $3 WHERE list_id = $1 AND item_id = $2;

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
