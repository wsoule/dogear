-- name: ListCategories :many
-- Every category with its live item count, computed in one grouped pass.
SELECT c.id, c.slug, c.name, c.parent_id, c.sort_order, COALESCE(n.item_count, 0)::int AS item_count
FROM categories c
LEFT JOIN (
  SELECT ic.category_id, count(*) AS item_count
  FROM item_categories ic JOIN items i ON i.id = ic.item_id
  WHERE i.status = 'live' AND i.link_status <> 'dead'
  GROUP BY ic.category_id
) n ON n.category_id = c.id
ORDER BY c.sort_order, c.name;

-- name: GetCategory :one
SELECT * FROM categories WHERE slug = $1;

-- name: CategoryChildren :many
SELECT id FROM categories WHERE parent_id = $1;

-- name: UpsertCategory :one
INSERT INTO categories (slug, name, parent_id, sort_order) VALUES ($1, $2, $3, $4)
ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
RETURNING id, (xmax = 0) AS inserted;

-- name: GetSourceList :one
SELECT * FROM source_lists WHERE repo = $1;

-- name: UpsertSourceList :one
INSERT INTO source_lists (repo, title, category_id, license, stars)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo) DO UPDATE SET title = EXCLUDED.title, category_id = EXCLUDED.category_id,
  license = EXCLUDED.license, stars = EXCLUDED.stars
WHERE source_lists.title IS DISTINCT FROM EXCLUDED.title
   OR source_lists.category_id IS DISTINCT FROM EXCLUDED.category_id
   OR source_lists.license IS DISTINCT FROM EXCLUDED.license
   OR source_lists.stars IS DISTINCT FROM EXCLUDED.stars
RETURNING id;

-- name: MarkSourceImported :exec
UPDATE source_lists SET readme_sha = $2, last_imported_at = now() WHERE id = $1;

-- name: InsertImportedItem :one
INSERT INTO items (url, url_norm, domain, title, blurb, via, status, kind)
VALUES ($1, $2, $3, $4, $5, 'import', 'live', $6)
ON CONFLICT (url_norm) DO NOTHING
RETURNING id;

-- name: ImportedItemByNorm :one
SELECT id, created_by, via, title, blurb, (like_count + dislike_count)::int AS votes FROM items WHERE url_norm = $1;

-- name: UpdateImportedText :execrows
UPDATE items SET title = $2, blurb = $3, updated_at = now()
WHERE id = $1 AND created_by IS NULL AND like_count = 0 AND dislike_count = 0
  AND (title IS DISTINCT FROM $2 OR blurb IS DISTINCT FROM $3)
  AND NOT EXISTS (SELECT 1 FROM votes v WHERE v.item_id = items.id);

-- name: InsertItemSource :execrows
INSERT INTO item_sources (item_id, source_list_id, heading_path, original_description)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING;

-- name: InsertItemCategory :execrows
INSERT INTO item_categories (item_id, category_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: RecountListCount :execrows
UPDATE items SET list_count = sub.n
FROM (SELECT item_id, count(*)::int AS n FROM item_sources WHERE item_id = ANY(@ids::bigint[]) GROUP BY item_id) sub
WHERE items.id = sub.item_id AND items.list_count <> sub.n;

-- name: ImportedGitHubItems :many
SELECT id, url_norm FROM items WHERE via = 'import' AND domain = 'github.com';

-- name: ImportedItemsForRescore :many
SELECT id, github_stars, list_count, like_count, dislike_count, prior_score, score, via FROM items
WHERE via = 'import';

-- name: SetStars :exec
UPDATE items SET github_stars = $2 WHERE id = $1 AND github_stars IS DISTINCT FROM $2;

-- name: SetPriorAndScore :exec
UPDATE items SET prior_score = $2, score = $3 WHERE id = $1;
