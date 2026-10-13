-- +goose Up
-- Imports: a link someone brings in from a Pocket, Raindrop, bookmarks, or
-- YouTube export that is not on Dogear yet becomes a 'personal' item. It is
-- the importer's and unreviewed: it shows in archives and on its own item
-- page (never indexed), but never in feeds, search, topics, the sitemap, or
-- creator pages. Posting the same link later promotes the row to a normal
-- post, so saves pointing at it keep working.
ALTER TABLE items DROP CONSTRAINT items_status_check;
ALTER TABLE items ADD CONSTRAINT items_status_check
  CHECK (status IN ('live','pending','removed','personal'));

-- +goose Down
-- Personal items cannot exist without the new status; hide them the way a
-- removed post is hidden, keeping the rows so nobody's saves are lost.
UPDATE items SET status = 'removed', removed_at = COALESCE(removed_at, now()), updated_at = now()
WHERE status = 'personal';
ALTER TABLE items DROP CONSTRAINT items_status_check;
ALTER TABLE items ADD CONSTRAINT items_status_check
  CHECK (status IN ('live','pending','removed'));
