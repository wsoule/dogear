package service

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"dogear/internal/db"
	"dogear/internal/importfile"
)

// importDomain returns a fresh domain so tests sharing a database never see
// each other's links.
func importDomain() string {
	seq++
	return fmt.Sprintf("imp%d.example", seq)
}

func runImport(t *testing.T, s *Service, a *Actor, opts ImportOptions, entries ...importfile.Entry) ImportResult {
	t.Helper()
	res, err := s.ImportSaves(context.Background(), a, importfile.Result{Format: importfile.Pocket, Entries: entries}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestImportSaves(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 0)
	existing := liveItem(t, s, newUser(t, s, 1))
	ex, err := s.Q.GetItem(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	d := importDomain()
	blocked := importDomain()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO blocked_domains (domain, reason) VALUES ($1, 'test')`, blocked); err != nil {
		t.Fatal(err)
	}
	saved := time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)
	entries := []importfile.Entry{
		{URL: ex.Url, Title: "whatever", Tags: []string{"Reading"}},
		{URL: "https://" + d + "/essay?utm_source=pocket", Title: "A Good Essay", Note: "read twice", Tags: []string{"Reading", "Essays"}, SavedAt: saved},
		{URL: "https://" + d + "/no-title", Title: "https://" + d + "/no-title"},
		{URL: "https://" + d + "/essay", Tags: []string{"later"}}, // same link as the second, merged
		{URL: "javascript:alert(1)"},
		{URL: "https://bit.ly/abc"},
		{URL: "https://" + blocked + "/x"},
	}
	res := runImport(t, s, me, ImportOptions{}, entries...)
	if res.Saved != 1 || res.Created != 2 || res.AlreadySaved != 0 || res.Skipped != 3 || res.TagsCreated != 3 || res.Tagged != 4 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.SkipSamples) != 3 {
		t.Fatalf("skip samples = %v", res.SkipSamples)
	}

	essay, err := s.Q.GetItemByNorm(ctx, "https://"+d+"/essay")
	if err != nil {
		t.Fatal(err)
	}
	if essay.Status != "personal" || essay.CreatedBy == nil || *essay.CreatedBy != me.ID() || essay.Title != "A Good Essay" || len(essay.Flags) != 0 {
		t.Fatalf("new item = %+v", essay)
	}
	untitled, err := s.Q.GetItemByNorm(ctx, "https://"+d+"/no-title")
	if err != nil {
		t.Fatal(err)
	}
	if untitled.Title != d+"/no-title" || !slices.Equal(untitled.Flags, []string{"untitled"}) {
		t.Fatalf("untitled item: title %q flags %v", untitled.Title, untitled.Flags)
	}
	sv, err := s.Q.GetSave(ctx, db.GetSaveParams{UserID: me.ID(), ItemID: essay.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !sv.SavedAt.Equal(saved) || sv.Note != "read twice" {
		t.Fatalf("save = %+v", sv)
	}
	// Imported tags are private unless asked otherwise.
	lists, err := s.MyLists(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]List{}
	for _, l := range lists {
		got[l.Slug] = l
	}
	if got["reading"].Visibility != "private" || got["reading"].Count != 2 || got["essays"].Count != 1 || got["later"].Count != 1 {
		t.Fatalf("tags = %+v", got)
	}

	// The same file again adds nothing.
	again := runImport(t, s, me, ImportOptions{}, entries...)
	if again.Saved != 0 || again.Created != 0 || again.AlreadySaved != 3 || again.TagsCreated != 0 || again.Tagged != 0 {
		t.Fatalf("re-import = %+v", again)
	}

	// A second person importing the same new link saves the first one's
	// item instead of making another.
	other := newUser(t, s, 0)
	r2 := runImport(t, s, other, ImportOptions{PublicTags: true}, importfile.Entry{URL: "https://" + d + "/essay", Tags: []string{"faves"}})
	if r2.Saved != 1 || r2.Created != 0 {
		t.Fatalf("second importer = %+v", r2)
	}
	ol, _ := s.MyLists(ctx, other)
	if i := slices.IndexFunc(ol, func(l List) bool { return l.Slug == "faves" }); i < 0 || ol[i].Visibility != "public" {
		t.Fatalf("public tags option: %+v", ol)
	}
}

func TestImportArchiveLimit(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 0)
	d := importDomain()
	// Fill the archive to one short of the limit.
	if _, err := s.Pool.Exec(ctx, `
	  WITH it AS (
	    INSERT INTO items (url, url_norm, domain, title, status, created_by)
	    SELECT 'https://'||$1||'/f'||g, 'https://'||$1||'/f'||g, $1, 't', 'personal', $2
	    FROM generate_series(1, $3::int) g RETURNING id)
	  INSERT INTO saves (user_id, item_id) SELECT $2, id FROM it`, d, me.ID(), maxSavesPerUser-1); err != nil {
		t.Fatal(err)
	}
	res := runImport(t, s, me, ImportOptions{},
		importfile.Entry{URL: "https://" + d + "/f1"}, // already saved: does not use room
		importfile.Entry{URL: "https://" + d + "/new1"},
		importfile.Entry{URL: "https://" + d + "/new2"})
	if res.AlreadySaved != 1 || res.Created != 1 || res.Skipped != 1 || res.Stopped == "" {
		t.Fatalf("result = %+v", res)
	}
	if _, err := s.Q.GetItemByNorm(ctx, "https://"+d+"/new2"); !notFound(err) {
		t.Fatalf("an item past the limit should not be created: %v", err)
	}
}

func TestPersonalItemVisibility(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, viewer := newUser(t, s, 1), newUser(t, s, 1)
	d := importDomain()
	runImport(t, s, me, ImportOptions{PublicTags: true}, importfile.Entry{URL: "https://" + d + "/p", Title: "Mine", Tags: []string{"stuff"}})
	it, err := s.Q.GetItemByNorm(ctx, "https://"+d+"/p")
	if err != nil {
		t.Fatal(err)
	}

	// Others see it on the public archive, the public tag, and its page.
	if _, ok := archiveIDs(t, s, viewer, me)[it.ID]; !ok {
		t.Fatal("personal item missing from the public archive")
	}
	if _, ok := archiveIDs(t, s, nil, me)[it.ID]; !ok {
		t.Fatal("personal item missing from the signed-out archive")
	}
	lv, err := s.GetList(ctx, viewer, me.User.Username, "stuff")
	if err != nil || len(lv.Entries) != 1 {
		t.Fatalf("tag view: %+v %v", lv.Entries, err)
	}
	page, err := s.GetItem(ctx, viewer, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Personal() || page.Poster != "" || s.Indexable(ctx, page) {
		t.Fatalf("item page: personal=%v poster=%q indexable=%v", page.Personal(), page.Poster, s.Indexable(ctx, page))
	}

	// Followers see it in /following, as on the archive, but the home
	// page's recent saves (a discovery surface) leave it out.
	if err := s.Follow(ctx, viewer, me.User.Username); err != nil {
		t.Fatal(err)
	}
	fv, err := s.FollowingFeed(ctx, viewer, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(fv.Saves, func(f FollowedSave) bool { return f.Item.ID == it.ID }) {
		t.Fatal("personal item missing from /following")
	}
	recent, err := s.RecentSaves(ctx, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(recent, func(r RecentSave) bool { return r.ItemID == it.ID }) {
		t.Fatal("personal item in recent public saves")
	}

	// Never in feeds or search, never on the importer's "posted" list, and
	// not votable.
	for _, fq := range []FeedQuery{{Sort: "new"}, {Sort: "top"}, {Q: "Mine", Sort: "new"}} {
		fp, err := s.Feed(ctx, viewer, fq)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fp.Items {
			if f.ID == it.ID {
				t.Fatalf("personal item in feed %+v", fq)
			}
		}
	}
	posts, err := s.UserItems(ctx, me, me.ID(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 0 {
		t.Fatalf("imports listed as posts: %+v", posts)
	}
	if _, err := s.Vote(ctx, viewer, it.ID, 1); err == nil {
		t.Fatal("vote on a personal item should fail")
	}

	// A shadow-banned importer's items are hidden from others, like posts.
	if _, err := s.Pool.Exec(ctx, `UPDATE users SET shadow_banned_at = now() WHERE id = $1`, me.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetItem(ctx, viewer, it.ID); err == nil {
		t.Fatal("shadow-banned importer's item should be hidden")
	}
}

func TestPostingPromotesPersonalItem(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	importer, poster := newUser(t, s, 0), newUser(t, s, 1)
	d := importDomain()
	link := "https://" + d + "/promote-me"
	runImport(t, s, importer, ImportOptions{}, importfile.Entry{URL: link})
	before, err := s.Q.GetItemByNorm(ctx, link)
	if err != nil {
		t.Fatal(err)
	}

	it, err := s.CreateItem(ctx, poster, NewItem{URL: link, Title: "Promote me", Blurb: "A link worth posting for real.", Creator: "Someone"})
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != before.ID || it.Status == "personal" || it.Title != "Promote me" || it.CreatedByID() != poster.ID() {
		t.Fatalf("promoted item = %+v", it)
	}
	after, _ := s.Q.GetItem(ctx, it.ID)
	if slices.Contains(after.Flags, "untitled") {
		t.Fatalf("flags after promotion: %v", after.Flags)
	}
	if _, ok := s.SavedNote(ctx, importer, it.ID); !ok {
		t.Fatal("importer's save should survive promotion")
	}
	if _, ok := s.SavedNote(ctx, poster, it.ID); !ok {
		t.Fatal("poster should have it saved")
	}
	// Now it is a post: posting it again is a duplicate.
	if _, err := s.CreateItem(ctx, newUser(t, s, 1), NewItem{URL: link, Title: "Again", Blurb: "A link worth posting for real."}); err == nil {
		t.Fatal("second post should be a duplicate")
	} else if e, ok := AsError(err); !ok || e.Code != "duplicate" || e.ItemID != it.ID {
		t.Fatalf("err = %v", err)
	}
}

func TestImportFallbackTitle(t *testing.T) {
	tests := []struct{ raw, norm, domain, want string }{
		{"", "https://example.com/a/b%20c", "example.com", "example.com/a/b c"},
		{"", "https://example.com/", "example.com", "example.com"},
		{"https://example.com/x", "https://example.com/x", "example.com", "example.com/x"},
		{"", "https://youtube.com/watch?v=dQw4w9WgXcQ", "youtube.com", "YouTube video dQw4w9WgXcQ"},
		{"  Real   title ", "https://example.com/x", "example.com", "Real title"},
	}
	for _, tt := range tests {
		got, _ := importTitle(tt.raw, tt.norm, tt.domain)
		if got != tt.want {
			t.Errorf("importTitle(%q, %q) = %q, want %q", tt.raw, tt.norm, got, tt.want)
		}
	}
}

func TestUntitledImportsGetBackfilled(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 1)
	d := importDomain()
	runImport(t, s, me, ImportOptions{}, importfile.Entry{URL: "https://" + d + "/page"}, importfile.Entry{URL: "https://" + d + "/named", Title: "Named"})
	untitled, _ := s.Q.GetItemByNorm(ctx, "https://"+d+"/page")
	named, _ := s.Q.GetItemByNorm(ctx, "https://"+d+"/named")

	// The backfill picks up every import (titles from a file are one
	// person's), never as trusted, so it cannot add creator pages.
	rows, err := s.Q.ItemsNeedingMedia(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, r := range rows {
		if r.ID == untitled.ID || r.ID == named.ID {
			found++
			if r.Trusted {
				t.Fatal("personal item backfilled as trusted")
			}
		}
	}
	if found != 2 {
		t.Fatalf("imports in media backfill = %d, want 2", found)
	}

	// A found title replaces a personal item's title, never a live one's,
	// which was reviewed as posted.
	live, err := s.Q.GetItem(ctx, liveItem(t, s, newUser(t, s, 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{untitled.ID, named.ID, live.ID} {
		if err := s.Q.SetItemDetails(ctx, db.SetItemDetailsParams{ID: id, Title: lookupTitle("The Real Title"), Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	untitled, _ = s.Q.GetItem(ctx, untitled.ID)
	named, _ = s.Q.GetItem(ctx, named.ID)
	after, _ := s.Q.GetItem(ctx, live.ID)
	if untitled.Title != "The Real Title" || len(untitled.Flags) != 0 || named.Title != "The Real Title" || after.Title != live.Title {
		t.Fatalf("after backfill: %q %v, %q, live %q", untitled.Title, untitled.Flags, named.Title, after.Title)
	}
}

// An import reaches only what a normal save could: someone else's pending
// post, a shadow-banned poster's item, and a blocked poster's item are
// skipped, not saved.
func TestImportSkipsHiddenItems(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me := newUser(t, s, 0)
	urlOf := func(id int64) string {
		it, err := s.Q.GetItem(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return it.Url
	}
	pending := liveItem(t, s, newUser(t, s, 1))
	shadowed := newUser(t, s, 1)
	byShadowed := liveItem(t, s, shadowed)
	blockedUser := newUser(t, s, 1)
	byBlocked := liveItem(t, s, blockedUser)
	visible := liveItem(t, s, newUser(t, s, 1))
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`UPDATE items SET status = 'pending' WHERE id = $1`, pending},
		{`UPDATE users SET shadow_banned_at = now() WHERE id = $1`, shadowed.ID()},
	} {
		if _, err := s.Pool.Exec(ctx, q.sql, q.arg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)`, me.ID(), blockedUser.ID()); err != nil {
		t.Fatal(err)
	}
	res := runImport(t, s, me, ImportOptions{},
		importfile.Entry{URL: urlOf(pending)},
		importfile.Entry{URL: urlOf(byShadowed)},
		importfile.Entry{URL: urlOf(byBlocked)},
		importfile.Entry{URL: urlOf(visible)},
	)
	if res.Saved != 1 || res.Created != 0 || res.Skipped != 3 {
		t.Fatalf("result = %+v", res)
	}
	for _, id := range []int64{pending, byShadowed, byBlocked} {
		if _, err := s.Q.GetSave(ctx, db.GetSaveParams{UserID: me.ID(), ItemID: id}); err == nil {
			t.Fatalf("hidden item %d was saved", id)
		}
	}
}

// A personal item's page is visible to others only once someone shares it
// publicly, so items imported under a private tag can't be read by id.
func TestPersonalItemNeedsPublicSave(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	me, stranger := newUser(t, s, 0), newUser(t, s, 0)
	d := importDomain()
	runImport(t, s, me, ImportOptions{}, importfile.Entry{URL: "https://" + d + "/secret", Title: "Secret", Tags: []string{"Private stuff"}})
	it, err := s.Q.GetItemByNorm(ctx, "https://"+d+"/secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetItem(ctx, stranger, it.ID); err != ErrNotFound {
		t.Fatalf("privately imported item visible to a stranger: err = %v", err)
	}
	if _, err := s.GetItem(ctx, nil, it.ID); err != ErrNotFound {
		t.Fatalf("privately imported item visible signed out: err = %v", err)
	}
	if _, err := s.GetItem(ctx, me, it.ID); err != nil {
		t.Fatalf("importer can't see own item: %v", err)
	}
	// Someone else importing it untagged shares it publicly.
	runImport(t, s, newUser(t, s, 0), ImportOptions{}, importfile.Entry{URL: "https://" + d + "/secret"})
	if _, err := s.GetItem(ctx, stranger, it.ID); err != nil {
		t.Fatalf("publicly saved item hidden: %v", err)
	}
}
