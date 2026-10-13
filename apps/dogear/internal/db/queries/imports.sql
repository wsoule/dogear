-- name: LockUserForImport :exec
-- Serializes one user's imports so two uploads at once cannot both pass
-- the archive and tag limits.
SELECT id FROM users WHERE id = $1 FOR UPDATE;

-- name: BlockedAmong :many
-- Which of these domains are blocked, directly or as a subdomain.
SELECT d::text AS domain FROM unnest(@domains::text[]) AS d
WHERE EXISTS (SELECT 1 FROM blocked_domains b WHERE d = b.domain OR d LIKE '%.' || b.domain);

-- name: ItemsByNorms :many
-- The items behind an import's links, with whether the importer may save
-- each one: the same rule as visibleTo (their own item, or a live or
-- personal one whose poster is not banned, shadow-banned, or blocked by
-- them), so an import cannot reach posts a normal save could not.
SELECT i.id, i.url_norm, i.status,
  (i.created_by = @user_id::bigint OR (i.status IN ('live','personal') AND (i.created_by IS NULL OR EXISTS (
     SELECT 1 FROM users u WHERE u.id = i.created_by AND u.shadow_banned_at IS NULL AND u.banned_at IS NULL
       AND NOT EXISTS (SELECT 1 FROM user_blocks b WHERE b.blocker_id = @user_id::bigint AND b.blocked_id = u.id)))))::bool AS visible
FROM items i WHERE i.url_norm = ANY(@norms::text[]);

-- name: BulkInsertPersonalItems :many
-- Adds an import's links that are not on Dogear yet as the importer's
-- personal items; a link someone else added meanwhile is left alone.
-- 'untitled' marks a title made up from the address, which the media
-- backfill replaces with the page's real title.
INSERT INTO items (url, url_norm, domain, title, created_by, created_via_token, via, status, kind, flags)
SELECT x.u, x.u, x.d, x.t, @created_by::bigint, sqlc.narg(token_id)::bigint, @via::text, 'personal', x.k,
  CASE WHEN x.untitled THEN '{untitled}'::text[] ELSE '{}'::text[] END
FROM (SELECT unnest(@urls::text[]) AS u, unnest(@domains::text[]) AS d, unnest(@titles::text[]) AS t,
             unnest(@kinds::text[]) AS k, unnest(@untitled::bool[]) AS untitled) AS x
ORDER BY x.u
ON CONFLICT (url_norm) DO NOTHING
RETURNING id, url_norm;

-- name: BulkInsertSaves :many
-- Saves imported items to the archive with the time the export says they
-- were saved. Existing saves, and their notes, are left as they are.
INSERT INTO saves (user_id, item_id, note, saved_at)
SELECT @user_id::bigint, x.i, x.n, x.a
FROM (SELECT unnest(@item_ids::bigint[]) AS i, unnest(@notes::text[]) AS n, unnest(@saved_at::timestamptz[]) AS a) AS x
ON CONFLICT (user_id, item_id) DO NOTHING
RETURNING item_id;

-- name: TagEntriesAmong :many
SELECT list_id, item_id FROM list_items
WHERE list_id = ANY(@list_ids::bigint[]) AND item_id = ANY(@item_ids::bigint[]);

-- name: BulkInsertTagEntries :execrows
-- Tags imported items, appending each tag's new entries after its existing
-- ones in file order (ord).
INSERT INTO list_items (list_id, item_id, position, added_at)
SELECT x.l, x.i, (COALESCE(m.mx, -1) + row_number() OVER (PARTITION BY x.l ORDER BY x.n))::int, x.a
FROM (SELECT unnest(@list_ids::bigint[]) AS l, unnest(@item_ids::bigint[]) AS i,
             unnest(@added_at::timestamptz[]) AS a, unnest(@ords::int[]) AS n) AS x
LEFT JOIN LATERAL (SELECT max(li.position) AS mx FROM list_items li WHERE li.list_id = x.l) m ON true
ON CONFLICT (list_id, item_id) DO NOTHING;

-- name: TouchLists :exec
UPDATE lists SET updated_at = now() WHERE id = ANY(@ids::bigint[]);

-- name: MarkListLinksDirty :exec
UPDATE list_links SET dirty_at = COALESCE(dirty_at, now()) WHERE list_id = ANY(@ids::bigint[]) AND status = 'active';

-- name: PromotePersonalItem :one
-- Turns a personal item into a post when someone posts its link, keeping
-- its id so every save of it still points at it.
UPDATE items SET url = @url, title = @title, blurb = @blurb, created_by = @created_by,
  created_via_token = sqlc.narg(created_via_token), via = @via, status = @status, kind = @kind,
  creator = @creator, flags = @flags, created_at = now(), updated_at = now()
WHERE id = @id AND status = 'personal'
RETURNING *;

-- name: PromotePersonalCreator :exec
-- Makes a personal item that turns out to be a creator's channel or
-- profile into the automatic creator page a trusted work links to.
UPDATE items SET status = 'live', via = 'auto', kind = @kind, title = @title,
  created_by = NULL, created_via_token = NULL, flags = '{}', created_at = now(), updated_at = now()
WHERE id = @id AND status = 'personal';

-- name: DeleteUnsharedPersonalItems :exec
-- On account deletion: personal items the user imported that nobody else
-- has saved go with them.
DELETE FROM items i WHERE i.created_by = $1 AND i.status = 'personal'
  AND NOT EXISTS (SELECT 1 FROM saves s WHERE s.item_id = i.id AND s.user_id <> $1);
