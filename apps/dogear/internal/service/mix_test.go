package service

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"dogear/internal/db"
)

func TestInterleaveKinds(t *testing.T) {
	var rows []db.Item
	for i := range 9 {
		rows = append(rows, db.Item{ID: int64(i + 1), Kind: "tool"})
	}
	picks := []db.Item{{ID: 100, Kind: "video"}, {ID: 101, Kind: "article"}, {ID: 102, Kind: "video"}, {ID: 103, Kind: "channel"}}
	got := interleaveKinds(rows, picks, 3)
	if len(got) != len(rows)+len(picks) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows)+len(picks))
	}
	var kinds []string
	run := 0
	for i, it := range got {
		kinds = append(kinds, it.Kind)
		if i > 0 && got[i-1].Kind == it.Kind {
			run++
		} else {
			run = 1
		}
		if run > 3 {
			t.Fatalf("run of %d %ss: %v", run, it.Kind, kinds)
		}
	}
	// Rows keep their score order.
	var order []int64
	for _, it := range got {
		if it.Kind == "tool" {
			order = append(order, it.ID)
		}
	}
	if !slices.IsSorted(order) {
		t.Fatalf("rows reordered: %v", order)
	}
	if out := interleaveKinds(rows, nil, 3); len(out) != len(rows) {
		t.Fatal("no picks should return rows unchanged")
	}
}

// TestMixedHomeFeedPaginates walks every page of the mixed home feed and
// checks it shows each item at most once, misses none of the plain feed's
// items, and puts videos on the first page even when they rank last.
func TestMixedHomeFeedPaginates(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	poster := newUser(t, s, 1)
	var mine []int64
	for range 12 {
		mine = append(mine, liveItem(t, s, poster))
	}
	var videos []int64
	for range 3 {
		seq++
		u := fmt.Sprintf("https://video%d.test/watch", seq)
		it, err := s.Q.InsertItem(ctx, db.InsertItemParams{Url: u, UrlNorm: u, Domain: "video.test", Title: "Talk",
			Blurb: "A talk worth watching.", CreatedBy: &poster.User.ID, Via: "web", Status: "live", Kind: "video", Flags: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		videos = append(videos, it.ID)
	}
	mine = append(mine, videos...)
	if _, err := s.Pool.Exec(ctx, "UPDATE items SET score = -1 WHERE id = ANY($1)", videos); err != nil {
		t.Fatal(err)
	}

	walk := func(mix bool) (first []Item, all []int64) {
		fq := FeedQuery{MixKinds: mix, Limit: 5}
		for page := 0; ; page++ {
			p, err := s.Feed(ctx, poster, fq)
			if err != nil {
				t.Fatal(err)
			}
			if page == 0 {
				first = p.Items
			}
			for _, it := range p.Items {
				all = append(all, it.ID)
			}
			if p.NextCursor == "" || page > 10_000 {
				return first, all
			}
			fq.Cursor = p.NextCursor
		}
	}
	first, mixed := walk(true)
	_, plain := walk(false)
	// Other tests share the database and may hold higher-ranked videos, so
	// check for three videos on the first page rather than these three.
	n := 0
	for _, it := range first {
		if it.Kind == "video" {
			n++
		}
	}
	if n < 3 {
		t.Errorf("first page has %d videos, want 3", n)
	}
	seen := map[int64]bool{}
	for _, id := range mixed {
		if seen[id] {
			t.Fatalf("item %d shown twice", id)
		}
		seen[id] = true
	}
	// Other test packages may add items while this runs, so compare only
	// this test's items: each must appear in both walks.
	for _, id := range mine {
		if !seen[id] || !slices.Contains(plain, id) {
			t.Errorf("item %d missing (mixed %t, plain %t)", id, seen[id], slices.Contains(plain, id))
		}
	}
}
