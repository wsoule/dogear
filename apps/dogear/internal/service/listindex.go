package service

import (
	"context"
	"fmt"

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
