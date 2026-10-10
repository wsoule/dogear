package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FeedParams selects one page of a feed. Feeds vary their filters and sort
// order, which sqlc cannot express, so this query is assembled by hand.
type FeedParams struct {
	ViewerID    *int64  // hides items from users the viewer blocked; shows their own shadow-banned items
	CategoryIDs []int32 // empty means every category
	Kind        string
	Sort        string // "top", "new", "stars", or "az"
	Query       string // trigram search over title, blurb, and domain
	IncludeDead bool
	CreatedBy   *int64 // a single user's posts (profile page)

	// Keyset cursor: the last row of the previous page.
	AfterScore   *float64
	AfterCreated *time.Time
	AfterStars   *int64
	AfterTitle   *string
	AfterID      int64

	Limit int
}

// Feed returns live items ordered by score or recency using keyset pagination.
func (q *Queries) Feed(ctx context.Context, p FeedParams) ([]Item, error) {
	var (
		where = []string{"i.status = 'live'"}
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if !p.IncludeDead {
		where = append(where, "i.link_status <> 'dead'")
	}
	// A category feed starts from that category's items and sorts them,
	// instead of walking the global score index until enough rows match,
	// which is slow for categories whose items rank low overall.
	with, join := "", ""
	if len(p.CategoryIDs) > 0 {
		with = "WITH cat AS MATERIALIZED (SELECT DISTINCT item_id FROM item_categories WHERE category_id = ANY(" + arg(p.CategoryIDs) + "::int[])) "
		// LATERAL forces the plan to start from the category's items and
		// look each up by primary key; otherwise the planner may walk the
		// global score index and rescan the CTE per row (minutes, not ms).
		// OFFSET 0 stops Postgres from flattening the subquery back into a join.
		join = "cat CROSS JOIN LATERAL (SELECT * FROM items WHERE items.id = cat.item_id OFFSET 0) i"
	}
	if p.Kind != "" {
		where = append(where, "i.kind = "+arg(p.Kind))
	}
	if p.CreatedBy != nil {
		where = append(where, "i.created_by = "+arg(*p.CreatedBy))
	}
	if q := strings.TrimSpace(p.Query); q != "" {
		where = append(where, "(i.title || ' ' || i.blurb || ' ' || i.domain) ILIKE "+arg("%"+escapeLike(q)+"%"))
	}
	if p.Sort == "new" {
		where = append(where, "i.via <> 'import'")
	}
	// Items by banned users are hidden; shadow-banned users' items are
	// visible only to themselves. NOT EXISTS (rather than a join) keeps the
	// score index usable.
	hidden := "u.banned_at IS NOT NULL OR u.shadow_banned_at IS NOT NULL"
	if p.ViewerID != nil {
		v := arg(*p.ViewerID)
		hidden = "u.banned_at IS NOT NULL OR (u.shadow_banned_at IS NOT NULL AND u.id <> " + v + ")"
		where = append(where,
			"(i.created_by IS NULL OR NOT EXISTS (SELECT 1 FROM user_blocks b WHERE b.blocker_id = "+v+" AND b.blocked_id = i.created_by))")
	}
	where = append(where, "(i.created_by IS NULL OR NOT EXISTS (SELECT 1 FROM users u WHERE u.id = i.created_by AND ("+hidden+")))")

	order := "i.score DESC, i.id DESC"
	switch p.Sort {
	case "new":
		order = "i.created_at DESC, i.id DESC"
		if p.AfterCreated != nil {
			where = append(where, "(i.created_at, i.id) < ("+arg(*p.AfterCreated)+", "+arg(p.AfterID)+")")
		}
	case "stars":
		order = "COALESCE(i.github_stars, -1) DESC, i.id DESC"
		if p.AfterStars != nil {
			where = append(where, "(COALESCE(i.github_stars, -1), i.id) < ("+arg(*p.AfterStars)+"::bigint, "+arg(p.AfterID)+")")
		}
	case "az":
		key := `regexp_replace(lower(%s), '^[^[:alnum:]]+', '') COLLATE "C"`
		order = fmt.Sprintf(key, "i.title") + " ASC, i.id ASC"
		if p.AfterTitle != nil {
			// The cursor carries only the last id; its sort key is looked up
			// here so it always matches the index expression exactly.
			id := arg(p.AfterID)
			where = append(where, "("+fmt.Sprintf(key, "i.title")+", i.id) > ((SELECT "+fmt.Sprintf(key, "t.title")+" FROM items t WHERE t.id = "+id+
				// Only publicly visible items can anchor a cursor, so a crafted
				// cursor cannot probe the titles of pending or hidden items.
				" AND t.status = 'live' AND (t.created_by IS NULL OR NOT EXISTS (SELECT 1 FROM users tu WHERE tu.id = t.created_by"+
				" AND (tu.banned_at IS NOT NULL OR tu.shadow_banned_at IS NOT NULL)))), "+id+")")
		}
	default:
		if p.AfterScore != nil {
			where = append(where, "(i.score, i.id) < ("+arg(*p.AfterScore)+"::float8, "+arg(p.AfterID)+")")
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 30
	}
	from := "items i"
	if join != "" {
		from = join
	}
	sql := with + `SELECT i.* FROM ` + from + ` WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY ` + order + ` LIMIT ` + arg(limit)

	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("feed query: %w", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Item])
	if err != nil {
		return nil, fmt.Errorf("feed scan: %w", err)
	}
	return items, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
