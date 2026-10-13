package service

import (
	"context"
	"testing"
)

// followingIDs returns the item ids on the viewer's first /following page.
func followingIDs(t *testing.T, s *Service, viewer *Actor) map[int64]FollowedSave {
	t.Helper()
	fv, err := s.FollowingFeed(context.Background(), viewer, "")
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]FollowedSave{}
	for _, e := range fv.Saves {
		out[e.Item.ID] = e
	}
	return out
}

func TestFollowRules(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, them, blocker := newUser(t, s, 1), newUser(t, s, 1), newUser(t, s, 1)

	if err := s.Follow(ctx, me, me.User.Username); err == nil {
		t.Fatal("following yourself should fail")
	}
	if err := s.Follow(ctx, nil, them.User.Username); err == nil {
		t.Fatal("signed out follow should fail")
	}
	readOnly := &Actor{User: me.User, Via: "api", Scopes: []string{"read"}}
	if err := s.Follow(ctx, readOnly, them.User.Username); err == nil {
		t.Fatal("a read-only token should not follow")
	}
	if err := s.Follow(ctx, me, "nobody-here-at-all"); err == nil {
		t.Fatal("following a missing user should fail")
	}
	for range 2 { // following twice is a no-op
		if err := s.Follow(ctx, me, them.User.Username); err != nil {
			t.Fatal(err)
		}
	}
	if !s.IsFollowing(ctx, me, them.ID()) || s.IsFollowing(ctx, them, me.ID()) {
		t.Fatal("follow should be one-way")
	}
	if followers, following, err := s.FollowCounts(ctx, them.ID()); err != nil || followers != 1 || following != 0 {
		t.Fatalf("counts = %d/%d, %v", followers, following, err)
	}
	p, err := s.GetProfile(ctx, me, them.User.Username)
	if err != nil {
		t.Fatal(err)
	}
	if !p.ViewerFollows || p.FollowerCount != 1 {
		t.Fatalf("profile follow state = %v/%d", p.ViewerFollows, p.FollowerCount)
	}
	if err := s.Unfollow(ctx, me, them.User.Username); err != nil {
		t.Fatal(err)
	}
	if s.IsFollowing(ctx, me, them.ID()) {
		t.Fatal("unfollow did not stick")
	}

	// A block in either direction refuses the follow, and blocking ends
	// existing follows both ways.
	if err := s.Follow(ctx, blocker, me.User.Username); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, blocker, me.User.Username, true); err != nil {
		t.Fatal(err)
	}
	if s.IsFollowing(ctx, blocker, me.ID()) {
		t.Fatal("blocking should end the blocker's follow")
	}
	if err := s.Follow(ctx, me, blocker.User.Username); err == nil {
		t.Fatal("following someone who blocked you should fail")
	}
	if err := s.Follow(ctx, blocker, me.User.Username); err == nil {
		t.Fatal("following someone you blocked should fail")
	}

	// Shadow-banned accounts cannot be followed by regular users.
	if err := s.Q.ShadowBanUser(ctx, them.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Follow(ctx, me, them.User.Username); err == nil {
		t.Fatal("following a shadow-banned user should fail")
	}
}

func TestFollowCap(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, them := newUser(t, s, 1), newUser(t, s, 1)
	// Fill the cap with follows of throwaway accounts, inserted directly.
	if _, err := s.Pool.Exec(ctx, `
		WITH made AS (
		  INSERT INTO users (username) SELECT $2 || '-' || g FROM generate_series(1, $3::int) g RETURNING id)
		INSERT INTO follows (follower_id, followee_id) SELECT $1, id FROM made`,
		me.ID(), "cap"+me.User.Username, maxFollowsPerUser); err != nil {
		t.Fatal(err)
	}
	err := s.Follow(ctx, me, them.User.Username)
	if e, ok := AsError(err); !ok || e.Code != "invalid" {
		t.Fatalf("follow past the cap = %v", err)
	}
}

func TestFollowingFeedVisibility(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, alice, bob, shadow, stranger := newUser(t, s, 1), newUser(t, s, 1), newUser(t, s, 1), newUser(t, s, 1), newUser(t, s, 1)
	poster := newUser(t, s, 1)
	for _, u := range []*Actor{alice, bob, shadow} {
		if err := s.Follow(ctx, me, u.User.Username); err != nil {
			t.Fatal(err)
		}
	}
	public, private, shared, shadowed, unfollowed, dead := liveItem(t, s, poster), liveItem(t, s, poster),
		liveItem(t, s, poster), liveItem(t, s, poster), liveItem(t, s, poster), liveItem(t, s, poster)
	priv := newTag(t, s, alice, "Secret", "private")
	pub := newTag(t, s, alice, "Good", "public")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveToList(ctx, alice, pub, public, "alice says yes", nil))
	must(s.SaveToList(ctx, alice, priv, private, "", nil))
	must(s.SaveItem(ctx, alice, shared, nil))
	must(s.SaveItem(ctx, bob, shared, nil))
	must(s.SaveItem(ctx, alice, dead, nil))
	must(s.SaveItem(ctx, shadow, shadowed, nil))
	must(s.SaveItem(ctx, stranger, unfollowed, nil))
	if _, err := s.Pool.Exec(ctx, "UPDATE items SET link_status = 'dead' WHERE id = $1", dead); err != nil {
		t.Fatal(err)
	}
	must(s.Q.ShadowBanUser(ctx, shadow.ID()))

	seen := followingIDs(t, s, me)
	for id, want := range map[int64]bool{public: true, private: false, shared: true, shadowed: false, unfollowed: false, dead: false} {
		if _, ok := seen[id]; ok != want {
			t.Errorf("item %d on /following = %v, want %v", id, ok, want)
		}
	}
	if e := seen[public]; e.Note != "alice says yes" || e.Username != alice.User.Username || len(e.Tags) != 1 || e.Tags[0].Slug != pub {
		t.Errorf("public entry = %+v", e)
	}
	// Bob saved shared last, so his entry leads and lists Alice too.
	if e := seen[shared]; len(e.SavedBy) != 2 || e.SavedBy[0] != bob.User.Username || e.SavedBy[1] != alice.User.Username {
		t.Errorf("shared saved_by = %v", e.SavedBy)
	}

	// Blocking a followed user hides their saves (and ends the follow).
	must(s.Block(ctx, me, bob.User.Username, true))
	if e := followingIDs(t, s, me)[shared]; len(e.SavedBy) != 1 || e.SavedBy[0] != alice.User.Username {
		t.Errorf("after blocking bob, shared saved_by = %v", e.SavedBy)
	}
	if _, err := s.FollowingFeed(ctx, nil, ""); err == nil {
		t.Fatal("signed-out /following should need sign-in")
	}
}

func TestFollowingFeedPages(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, a, b := newUser(t, s, 1), newUser(t, s, 2), newUser(t, s, 2)
	poster := newUser(t, s, 1)
	for _, u := range []*Actor{a, b} {
		if err := s.Follow(ctx, me, u.User.Username); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct items so nothing merges: 60 + 3 saves across two people.
	total := followingPageSize + 13
	for i := range total {
		saver := a
		if i%2 == 1 {
			saver = b
		}
		if err := s.SaveItem(ctx, saver, liveItem(t, s, poster), nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.FollowingFeed(ctx, me, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Saves) != followingPageSize || first.Next == "" {
		t.Fatalf("first page: %d saves, next %q", len(first.Saves), first.Next)
	}
	second, err := s.FollowingFeed(ctx, me, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Saves) != total-followingPageSize || second.Next != "" {
		t.Fatalf("second page: %d saves, next %q", len(second.Saves), second.Next)
	}
	seen := map[int64]bool{}
	for _, e := range append(first.Saves, second.Saves...) {
		if seen[e.Item.ID] {
			t.Fatalf("item %d on both pages", e.Item.ID)
		}
		seen[e.Item.ID] = true
	}
	if _, err := s.FollowingFeed(ctx, me, "1-2"); err == nil {
		t.Fatal("bad cursor should be rejected")
	}
}

func TestRecentSaves(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	viewer, trusted, newbie := newUser(t, s, 1), newUser(t, s, 1), newUser(t, s, 0)
	poster := newUser(t, s, 1)
	older, newer, hidden, fromNewbie := liveItem(t, s, poster), liveItem(t, s, poster), liveItem(t, s, poster), liveItem(t, s, poster)
	priv := newTag(t, s, trusted, "Mine", "private")
	for _, id := range []int64{older, newer} {
		if err := s.SaveItem(ctx, trusted, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveToList(ctx, trusted, priv, hidden, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveItem(ctx, newbie, fromNewbie, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentSaves(ctx, viewer, 20)
	if err != nil {
		t.Fatal(err)
	}
	var mine []int64
	for _, r := range got {
		if r.ItemID == fromNewbie {
			t.Error("trust-0 saves should not be on the home strip")
		}
		if r.Username == trusted.User.Username {
			mine = append(mine, r.ItemID)
		}
	}
	// One entry per person: the newest public save, never the private one.
	if len(mine) != 1 || mine[0] == hidden {
		t.Fatalf("trusted user's strip entries = %v (want only one public save)", mine)
	}
	if mine[0] != newer {
		t.Errorf("strip shows %d, want the newest public save %d", mine[0], newer)
	}
	// Blocking the saver hides them for that viewer.
	if err := s.Block(ctx, viewer, trusted.User.Username, true); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RecentSaves(ctx, viewer, 20)
	for _, r := range got {
		if r.Username == trusted.User.Username {
			t.Fatal("blocked saver still on the strip")
		}
	}
}

func TestSuggestedPeople(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, busy := newUser(t, s, 1), newUser(t, s, 2)
	poster := newUser(t, s, 1)
	for range 3 {
		if err := s.SaveItem(ctx, busy, liveItem(t, s, poster), nil); err != nil {
			t.Fatal(err)
		}
	}
	has := func() bool {
		ppl, err := s.SuggestedPeople(ctx, me, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ppl {
			if p.Username == busy.User.Username {
				return true
			}
			if p.Username == me.User.Username {
				t.Fatal("suggested yourself")
			}
		}
		return false
	}
	if !has() {
		t.Fatal("a trusted user with public saves should be suggested")
	}
	if err := s.Follow(ctx, me, busy.User.Username); err != nil {
		t.Fatal(err)
	}
	if has() {
		t.Fatal("people you follow should not be suggested")
	}
}
