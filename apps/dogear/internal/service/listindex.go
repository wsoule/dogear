package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"dogear/internal/db"
)

// AwesomeGroup is one top-level category and the awesome lists under it.
type AwesomeGroup struct {
	Name, Slug string
	Lists      []db.SourceListIndexRow
}

// ListIndex is everything shown on /lists: people's public lists and every
// imported awesome list, each of which is browsable as its own list page.
type ListIndex struct {
	People  []db.PublicListsRow
	Awesome []AwesomeGroup
	Total   int
}

// ListsIndex builds the /lists page data, cached for a minute.
func (s *Service) ListsIndex(ctx context.Context) (ListIndex, error) {
	if v, ok := s.indexCache.Get("lists"); ok {
		return v, nil
	}
	var out ListIndex
	people, err := s.Q.PublicLists(ctx)
	if err != nil {
		return out, fmt.Errorf("public lists: %w", err)
	}
	out.People = people
	rows, err := s.Q.SourceListIndex(ctx)
	if err != nil {
		return out, fmt.Errorf("source lists: %w", err)
	}
	for _, r := range rows {
		if r.ItemCount == 0 {
			continue
		}
		n := len(out.Awesome)
		if n == 0 || out.Awesome[n-1].Slug != r.ParentSlug {
			out.Awesome = append(out.Awesome, AwesomeGroup{Name: r.ParentName, Slug: r.ParentSlug})
			n++
		}
		out.Awesome[n-1].Lists = append(out.Awesome[n-1].Lists, r)
		out.Total++
	}
	s.indexCache.Set("lists", out)
	return out, nil
}

// SourceForCategory returns the awesome list a subcategory was imported
// from, or nil for seeded topic categories.
func (s *Service) SourceForCategory(ctx context.Context, slug string) *db.SourceListByCategoryRow {
	c, err := s.Q.GetCategory(ctx, slug)
	if err != nil || c.ParentID == nil {
		return nil
	}
	src, err := s.Q.SourceListByCategory(ctx, &c.ID)
	if err != nil {
		return nil
	}
	return &src
}

// ListSorts are the orders a single list can be viewed in: the owner's own
// order, score, most recently added, and title A–Z.
var ListSorts = []string{"owner", "top", "added", "az"}

// SortEntries reorders a list's entries for display. Unknown sorts keep the
// owner's order.
func SortEntries(entries []Entry, sort string) {
	switch sort {
	case "top":
		slices.SortStableFunc(entries, func(a, b Entry) int { return cmp.Compare(b.Item.Score, a.Item.Score) })
	case "added":
		slices.SortStableFunc(entries, func(a, b Entry) int { return b.AddedAt.Compare(a.AddedAt) })
	case "az":
		slices.SortStableFunc(entries, func(a, b Entry) int {
			return cmp.Compare(strings.ToLower(a.Item.Title), strings.ToLower(b.Item.Title))
		})
	}
}

// IndexSorts are the orders for the awesome lists on /lists.
var IndexSorts = []string{"name", "items", "stars"}

// SortIndex returns a copy of the index with each group's awesome lists and
// the people's lists reordered.
func SortIndex(idx ListIndex, sort string) ListIndex {
	out := idx
	out.Awesome = make([]AwesomeGroup, len(idx.Awesome))
	for i, g := range idx.Awesome {
		lists := slices.Clone(g.Lists)
		switch sort {
		case "items":
			slices.SortStableFunc(lists, func(a, b db.SourceListIndexRow) int { return cmp.Compare(b.ItemCount, a.ItemCount) })
		case "stars":
			star := func(r db.SourceListIndexRow) int32 {
				if r.Stars == nil {
					return -1
				}
				return *r.Stars
			}
			slices.SortStableFunc(lists, func(a, b db.SourceListIndexRow) int { return cmp.Compare(star(b), star(a)) })
		}
		out.Awesome[i] = AwesomeGroup{Name: g.Name, Slug: g.Slug, Lists: lists}
	}
	out.People = slices.Clone(idx.People)
	if sort == "items" {
		slices.SortStableFunc(out.People, func(a, b db.PublicListsRow) int { return cmp.Compare(b.EntryCount, a.EntryCount) })
	}
	return out
}
