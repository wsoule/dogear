package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dogear/internal/db"
)

// fakeChooser stands in for jev: it returns a fixed answer and records the
// options it was offered.
type fakeChooser struct {
	choice  string
	prob    float64
	err     error
	options map[string]string
}

func (f *fakeChooser) Choose(_ context.Context, _ any, _ string, options map[string]string) (string, float64, error) {
	f.options = options
	return f.choice, f.prob, f.err
}

// tagsOf returns the slugs of the user's tags on an item.
func tagsOf(t *testing.T, s *Service, a *Actor, itemID int64) []string {
	t.Helper()
	rows, err := s.Q.TagsForItems(context.Background(), db.TagsForItemsParams{UserID: a.ID(), Ids: []int64{itemID}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Slug)
	}
	return out
}

// Without jev, a dogear is filed by topic keywords: into a new public tag
// named after the topic, or the saver's existing tag of that name.
func TestAutoTagByKeywords(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	poster := newUser(t, s, 1)
	before, _ := s.Q.CountUserLists(ctx, me.ID())

	rust := liveItemTitled(t, s, poster, "Learning Rust programming the hard way")
	if on, err := s.ToggleDogear(ctx, me, rust); err != nil || !on {
		t.Fatalf("dogear = %v, %v", on, err)
	}
	if got := tagsOf(t, s, me, rust); len(got) != 1 || got[0] != "programming" {
		t.Fatalf("tags = %v, want [programming]", got)
	}
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: me.ID(), Slug: "programming"})
	if err != nil || l.Visibility != "public" || l.Title != "Programming" {
		t.Fatalf("auto tag = %+v, %v", l, err)
	}

	// The next programming save goes in the same tag, not a new one.
	golang := liveItemTitled(t, s, poster, "Concurrency in golang")
	if _, err := s.ToggleDogear(ctx, me, golang); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, s, me, golang); len(got) != 1 || got[0] != "programming" {
		t.Fatalf("tags = %v, want [programming]", got)
	}
	if n, _ := s.Q.CountUserLists(ctx, me.ID()); n != before+1 {
		t.Fatalf("tags created = %d, want 1", n-before)
	}

	// Nothing matches: saved, but left untagged.
	plain := liveItemTitled(t, s, poster, "A quiet thing")
	if _, err := s.ToggleDogear(ctx, me, plain); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, s, me, plain); len(got) != 0 {
		t.Fatalf("tags = %v, want none", got)
	}
}

// With jev, its pick decides: an existing tag, or a topic to start one; when
// it is unsure or fails, keywords decide; a tagged save is left alone.
func TestAutoTagWithJev(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	poster := newUser(t, s, 1)
	if _, err := s.CreateList(ctx, me, ListInput{Title: "Read later", Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeChooser{choice: "tag:read-later", prob: 0.9}
	s.Grouper = f

	a := liveItemTitled(t, s, poster, "A long essay")
	if _, err := s.ToggleDogear(ctx, me, a); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, s, me, a); len(got) != 1 || got[0] != "read-later" {
		t.Fatalf("tags = %v, want [read-later]", got)
	}
	if _, ok := f.options["tag:read-later"]; !ok {
		t.Fatalf("jev was not offered the saver's tag: %v", f.options)
	}
	if _, ok := f.options["new:design"]; !ok {
		t.Fatalf("jev was not offered topics for a new tag: %v", f.options)
	}

	f.choice, f.prob = "new:design", 0.8
	b := liveItemTitled(t, s, poster, "Another thing")
	if _, err := s.ToggleDogear(ctx, me, b); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, s, me, b); len(got) != 1 || got[0] != "design" {
		t.Fatalf("tags = %v, want [design]", got)
	}

	// Unsure: the keyword fallback files it.
	f.choice, f.prob = "tag:read-later", 0.3
	c := liveItemTitled(t, s, poster, "Notes on typography")
	if _, err := s.ToggleDogear(ctx, me, c); err != nil {
		t.Fatal(err)
	}
	if got := tagsOf(t, s, me, c); len(got) != 1 || got[0] != "design" {
		t.Fatalf("unsure jev: tags = %v, want [design]", got)
	}

	// jev down: keywords again, and the save still happens.
	f.err = errors.New("529 overloaded")
	d := liveItemTitled(t, s, poster, "Machine learning basics")
	if on, err := s.ToggleDogear(ctx, me, d); err != nil || !on {
		t.Fatalf("dogear with jev down = %v, %v", on, err)
	}
	if got := tagsOf(t, s, me, d); len(got) != 1 || got[0] != "ai" {
		t.Fatalf("jev down: tags = %v, want [ai]", got)
	}

	// Already tagged by hand: AutoTag leaves it.
	f.err, f.choice, f.prob = nil, "new:music", 0.99
	e := liveItemTitled(t, s, poster, "Something")
	if err := s.SaveToList(ctx, me, "read-later", e, "", nil); err != nil {
		t.Fatal(err)
	}
	if slug, err := s.AutoTag(ctx, me, e); err != nil || slug != "" {
		t.Fatalf("auto tag on a tagged save = %q, %v", slug, err)
	}
}

// QuickSave takes just a link: one already on Dogear is saved as it is, a new
// one becomes a personal item, and both are filed by AutoTag.
func TestQuickSave(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	existing := liveItemTitled(t, s, newUser(t, s, 1), "Intro to databases")
	ex, err := s.Q.GetItem(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	id, tag, err := s.QuickSave(ctx, me, ex.Url)
	if err != nil || id != existing || tag != "programming" {
		t.Fatalf("quick save existing = %d %q %v", id, tag, err)
	}

	seq++
	fresh := fmt.Sprintf("https://quick%d.test/post", seq)
	id, _, err = s.QuickSave(ctx, me, fresh)
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Q.GetItem(ctx, id)
	if err != nil || it.Status != "personal" {
		t.Fatalf("new link = %+v, %v", it, err)
	}
	if _, err := s.Q.GetSave(ctx, db.GetSaveParams{UserID: me.ID(), ItemID: id}); err != nil {
		t.Fatalf("new link not saved: %v", err)
	}

	if _, _, err := s.QuickSave(ctx, me, "not a link"); err == nil {
		t.Fatal("quick save of a non-link succeeded")
	}
}

// GroupUntagged files saves made before auto-tagging and leaves tagged and
// unmatched ones alone.
func TestGroupUntagged(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	poster := newUser(t, s, 1)
	rust := liveItemTitled(t, s, poster, "Rust programming tips")
	plain := liveItemTitled(t, s, poster, "A quiet thing")
	for _, id := range []int64{rust, plain} {
		if err := s.SaveItem(ctx, me, id, nil); err != nil { // no auto-tag on SaveItem
			t.Fatal(err)
		}
	}
	res, err := s.GroupUntagged(ctx, me)
	if err != nil || res.Looked != 2 || res.Tagged != 1 {
		t.Fatalf("group = %+v, %v", res, err)
	}
	if got := tagsOf(t, s, me, rust); len(got) != 1 || got[0] != "programming" {
		t.Fatalf("tags = %v, want [programming]", got)
	}
	// A second run only looks at what's still untagged.
	if res, _ := s.GroupUntagged(ctx, me); res.Looked != 1 || res.Tagged != 0 {
		t.Fatalf("second run = %+v", res)
	}
}
