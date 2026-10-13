-- name: UpsertSave :one
-- Saves an item to the user's archive. A NULL note leaves an existing note
-- alone; inserted reports whether the save is new.
INSERT INTO saves (user_id, item_id, note)
VALUES ($1, $2, COALESCE(sqlc.narg(note)::text, ''))
ON CONFLICT (user_id, item_id) DO UPDATE SET
  note = COALESCE(sqlc.narg(note)::text, saves.note),
  updated_at = CASE WHEN sqlc.narg(note)::text IS NOT NULL AND sqlc.narg(note)::text <> saves.note
    THEN now() ELSE saves.updated_at END
RETURNING (xmax = 0)::bool AS inserted;

-- name: GetSave :one
SELECT * FROM saves WHERE user_id = $1 AND item_id = $2;

-- name: DeleteSave :exec
DELETE FROM saves WHERE user_id = $1 AND item_id = $2;

-- name: CountSaves :one
SELECT count(*) FROM saves WHERE user_id = $1;

-- name: UntagEverywhere :many
-- Removes an item from all of a user's tags; returns the affected tags.
DELETE FROM list_items li USING lists l
WHERE l.id = li.list_id AND l.user_id = $1 AND li.item_id = $2
RETURNING li.list_id;

-- name: SavedAmong :many
-- Which of these items the user has saved, and how many tags each has.
SELECT s.item_id,
  (SELECT count(*) FROM list_items li JOIN lists l ON l.id = li.list_id
   WHERE l.user_id = s.user_id AND li.item_id = s.item_id)::int AS tag_count
FROM saves s WHERE s.user_id = $1 AND s.item_id = ANY(@ids::bigint[]);

-- name: ArchivePage :many
-- One page of a user's archive, newest first, keyset-paginated on
-- (saved_at, item_id). With public_only, a save is hidden when any of its
-- tags is not public, so private tags never leak through the archive.
SELECT s.note, s.saved_at, sqlc.embed(i)
FROM saves s JOIN items i ON i.id = s.item_id
WHERE s.user_id = @user_id
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (s.saved_at, s.item_id) < (sqlc.narg(before_at)::timestamptz, @before_id::bigint))
  AND (NOT @public_only::bool OR NOT EXISTS (
       SELECT 1 FROM list_items li JOIN lists l ON l.id = li.list_id
       WHERE l.user_id = s.user_id AND li.item_id = s.item_id AND l.visibility <> 'public'))
ORDER BY s.saved_at DESC, s.item_id DESC
LIMIT @lim;

-- name: TagsForItems :many
-- The user's tags on each of these items, in the user's tag order.
SELECT li.item_id, l.slug, l.title, l.visibility
FROM list_items li JOIN lists l ON l.id = li.list_id
WHERE l.user_id = $1 AND li.item_id = ANY(@ids::bigint[])
ORDER BY l.sort_order, l.id;

-- name: SetSaveNote :exec
UPDATE saves SET note = $3, updated_at = now() WHERE user_id = $1 AND item_id = $2 AND note <> $3;

-- name: UserSaves :many
-- Every save, for account export.
SELECT s.note, s.saved_at, i.id, i.url, i.title
FROM saves s JOIN items i ON i.id = s.item_id
WHERE s.user_id = $1 ORDER BY s.saved_at;

-- name: MarkTagLinksDirtyForItem :exec
-- Flags loggit links on every tag of the user's that holds the item.
UPDATE list_links k SET dirty_at = COALESCE(k.dirty_at, now())
FROM list_items li
WHERE li.list_id = k.list_id AND k.user_id = $1 AND li.item_id = $2 AND k.status = 'active';

-- name: UpsertSaveFromSync :one
-- Saves an item pulled in by loggit sync. An existing non-empty note wins
-- over the incoming one; returns the note the save ends up with.
INSERT INTO saves (user_id, item_id, note) VALUES ($1, $2, $3)
ON CONFLICT (user_id, item_id) DO UPDATE SET
  note = CASE WHEN saves.note = '' THEN EXCLUDED.note ELSE saves.note END
RETURNING note;

-- name: CountPublicSaves :one
-- How many saves a signed-out visitor sees in a user's archive: live items
-- with working links whose tags are all public (as ArchivePage public_only).
SELECT count(*) FROM saves s JOIN items i ON i.id = s.item_id
WHERE s.user_id = $1 AND i.status = 'live' AND i.link_status <> 'dead'
  AND NOT EXISTS (
       SELECT 1 FROM list_items li JOIN lists l ON l.id = li.list_id
       WHERE l.user_id = s.user_id AND li.item_id = s.item_id AND l.visibility <> 'public');
