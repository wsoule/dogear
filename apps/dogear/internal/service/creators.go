package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dogear/internal/db"
)

// SearchCreators finds channels and people for a search: those whose name
// matches q first, then the creators of the matching works (results), most
// represented first. It returns at most n, as the viewer may see them.
func (s *Service) SearchCreators(ctx context.Context, viewer *Actor, q string, results []Item, n int) ([]Item, error) {
	q = strings.TrimSpace(q)
	if q == "" || n <= 0 {
		return nil, nil
	}
	named, err := s.Q.CreatorsByName(ctx, db.CreatorsByNameParams{Q: escapeLike(q), Lim: int32(n)})
	if err != nil {
		return nil, fmt.Errorf("creators by name: %w", err)
	}
	rows := slices.Clone(named)
	seen := map[int64]bool{}
	for _, r := range named {
		seen[r.ID] = true
	}
	// Creators of matching works, ordered by how many of the results are
	// theirs (first appearance breaks ties, keeping relevance order).
	counts := map[int64]int{}
	var order []int64
	for _, it := range results {
		if it.CreatorID == 0 || seen[it.CreatorID] {
			continue
		}
		if counts[it.CreatorID] == 0 {
			order = append(order, it.CreatorID)
		}
		counts[it.CreatorID]++
	}
	slices.SortStableFunc(order, func(a, b int64) int { return counts[b] - counts[a] })
	if room := n - len(rows); room > 0 && len(order) > 0 {
		byWork, err := s.Q.ItemsByIDs(ctx, order[:min(room, len(order))])
		if err != nil {
			return nil, fmt.Errorf("creators of works: %w", err)
		}
		pos := map[int64]int{}
		for i, id := range order {
			pos[id] = i
		}
		slices.SortFunc(byWork, func(a, b db.Item) int { return pos[a.ID] - pos[b.ID] })
		for _, r := range byWork {
			if r.Status == "live" && r.LinkStatus != "dead" {
				rows = append(rows, r)
			}
		}
	}
	var keep []db.Item
	for _, r := range rows {
		if ok, err := s.visibleTo(ctx, viewer, r); err == nil && ok {
			keep = append(keep, r)
		}
	}
	return s.decorate(ctx, viewer, keep)
}

// escapeLike escapes the LIKE wildcards in user input.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
