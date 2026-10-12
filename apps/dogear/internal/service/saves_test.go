package service

import (
	"context"
	"testing"
)

func newTag(t *testing.T, s *Service, a *Actor, title, vis string) string {
	t.Helper()
	l, err := s.CreateList(context.Background(), a, ListInput{Title: title, Visibility: vis})
	if err != nil {
		t.Fatal(err)
	}
	return l.Slug
}

func archiveIDs(t *testing.T, s *Service, viewer *Actor, owner *Actor) map[int64]Entry {
	t.Helper()
	av, err := s.Archive(context.Background(), viewer, owner.ID(), "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]Entry{}
	for _, e := range av.Entries {
		out[e.Item.ID] = e
	}
	return out
}

func TestTaggingSavesAndSharesOneNote(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	id := liveItem(t, s, newUser(t, s, 1))
	ai, essays := newTag(t, s, me, "AI", "public"), newTag(t, s, me, "Essays", "public")

	if err := s.SaveToList(ctx, me, ai, id, "best primer", nil); err != nil {
		t.Fatal(err)
	}
	if note, ok := s.SavedNote(ctx, me, id); !ok || note != "best primer" {
		t.Fatalf("tagging should save with the note, got %q saved=%v", note, ok)
	}
	// An empty note when tagging again keeps the existing note.
	if err := s.SaveToList(ctx, me, essays, id, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{ai, essays} {
		lv, err := s.GetList(ctx, me, me.User.Username, slug)
		if err != nil {
			t.Fatal(err)
		}
		if len(lv.Entries) != 1 || lv.Entries[0].Note != "best primer" {
			t.Fatalf("%s entries = %+v", slug, lv.Entries)
		}
	}
	// Editing the note changes it on every tag.
	edited := "send to anyone new"
	if err := s.SaveItem(ctx, me, id, &edited); err != nil {
		t.Fatal(err)
	}
	lv, _ := s.GetList(ctx, me, me.User.Username, essays)
	if lv.Entries[0].Note != edited {
		t.Fatalf("note on essays = %q", lv.Entries[0].Note)
	}
}

func TestUntagKeepsSaveAndUnsaveUntags(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	id := liveItem(t, s, newUser(t, s, 1))
	ai := newTag(t, s, me, "AI", "public")
	if err := s.SaveToList(ctx, me, ai, id, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFromList(ctx, me, ai, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.SavedNote(ctx, me, id); !ok {
		t.Fatal("untagging should keep the save")
	}
	if err := s.SaveToList(ctx, me, ai, id, "", nil); err != nil {
		t.Fatal(err)
	}
	on, err := s.ToggleDogear(ctx, me, id)
	if err != nil || on {
		t.Fatalf("toggle off: on=%v err=%v", on, err)
	}
	if _, ok := s.SavedNote(ctx, me, id); ok {
		t.Fatal("save should be gone")
	}
	if tags, _ := s.SavedIn(ctx, me, id); len(tags) != 0 {
		t.Fatalf("unsave should untag everywhere, still in %v", tags)
	}
	on, err = s.ToggleDogear(ctx, me, id)
	if err != nil || !on {
		t.Fatalf("toggle on: on=%v err=%v", on, err)
	}
}

func TestArchiveHidesSavesInNonPublicTags(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, other := newUser(t, s, 1), newUser(t, s, 1)
	poster := newUser(t, s, 1)
	untagged, public, private, unlisted, mixed := liveItem(t, s, poster), liveItem(t, s, poster),
		liveItem(t, s, poster), liveItem(t, s, poster), liveItem(t, s, poster)
	pub, priv, unl := newTag(t, s, me, "Pub", "public"), newTag(t, s, me, "Priv", "private"), newTag(t, s, me, "Unl", "unlisted")

	if err := s.SaveItem(ctx, me, untagged, nil); err != nil {
		t.Fatal(err)
	}
	for _, tg := range []struct {
		slug string
		id   int64
	}{{pub, public}, {priv, private}, {unl, unlisted}, {pub, mixed}, {priv, mixed}} {
		if err := s.SaveToList(ctx, me, tg.slug, tg.id, "", nil); err != nil {
			t.Fatal(err)
		}
	}

	seen := archiveIDs(t, s, other, me)
	for id, want := range map[int64]bool{untagged: true, public: true, private: false, unlisted: false, mixed: false} {
		if _, ok := seen[id]; ok != want {
			t.Errorf("item %d visible to others = %v, want %v", id, ok, want)
		}
	}
	if tags := seen[public].Tags; len(tags) != 1 || tags[0].Slug != pub {
		t.Errorf("public entry tags = %+v", tags)
	}
	if n := len(archiveIDs(t, s, nil, me)); n != 2 {
		t.Errorf("signed-out viewer sees %d saves, want 2", n)
	}
	mine := archiveIDs(t, s, me, me)
	if len(mine) != 5 {
		t.Fatalf("owner sees %d saves, want 5", len(mine))
	}
	if tags := mine[mixed].Tags; len(tags) != 2 {
		t.Errorf("owner should see both tags on mixed, got %+v", tags)
	}
}

func TestArchivePages(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 2)
	poster := newUser(t, s, 1)
	total := archivePageSize + 3
	for range total {
		if err := s.SaveItem(ctx, me, liveItem(t, s, poster), nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Archive(ctx, me, me.ID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != archivePageSize || first.Next == "" {
		t.Fatalf("first page: %d entries, next %q", len(first.Entries), first.Next)
	}
	second, err := s.Archive(ctx, me, me.ID(), first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 3 || second.Next != "" {
		t.Fatalf("second page: %d entries, next %q", len(second.Entries), second.Next)
	}
	seen := map[int64]bool{}
	for _, e := range append(first.Entries, second.Entries...) {
		if seen[e.Item.ID] {
			t.Fatalf("item %d on both pages", e.Item.ID)
		}
		seen[e.Item.ID] = true
	}
	if _, err := s.Archive(ctx, me, me.ID(), "garbage"); err == nil {
		t.Fatal("bad cursor should be rejected")
	}
}

func TestArchiveTokenWithoutReadGetsPublicView(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	id := liveItem(t, s, newUser(t, s, 1))
	priv := newTag(t, s, me, "Priv", "private")
	if err := s.SaveToList(ctx, me, priv, id, "", nil); err != nil {
		t.Fatal(err)
	}
	postOnly := &Actor{User: me.User, Via: "api", Scopes: []string{"post"}}
	if seen := archiveIDs(t, s, postOnly, me); len(seen) != 0 {
		t.Fatalf("post-only token saw %d private saves", len(seen))
	}
}

func TestDogearKeepsFilingIntoPrivateDogearedTag(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, other := newUser(t, s, 1), newUser(t, s, 1)
	id := liveItem(t, s, newUser(t, s, 1))
	newTag(t, s, me, "Dogeared", "private")
	if on, err := s.ToggleDogear(ctx, me, id); err != nil || !on {
		t.Fatalf("toggle: on=%v err=%v", on, err)
	}
	if tags, _ := s.SavedIn(ctx, me, id); len(tags) != 1 || tags[0] != legacyDogearSlug {
		t.Fatalf("tags = %v, want the private dogeared tag", tags)
	}
	if seen := archiveIDs(t, s, other, me); len(seen) != 0 {
		t.Fatal("a dogear into a private Dogeared tag should stay hidden")
	}
}
