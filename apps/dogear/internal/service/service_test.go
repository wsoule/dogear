package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/migrations"
)

// newTestService connects to TEST_DATABASE_URL (a throwaway database that
// gets migrated) and skips the test when it is not set.
func newTestService(t *testing.T) *Service {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := &config.Config{Env: "development", BaseURL: "http://test", Signups: "open", IPHashSalt: "x"}
	return New(pool, cfg, classify.Noop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var seq = time.Now().UnixNano() % 1_000_000

func newUser(t *testing.T, s *Service, trust int16) *Actor {
	t.Helper()
	seq++
	name := fmt.Sprintf("u%d", seq)
	u, err := s.SignUp(context.Background(), ExternalIdentity{Provider: "github", ProviderUserID: name}, name, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if trust > 0 {
		if err := s.SetTrust(context.Background(), u.ID, trust); err != nil {
			t.Fatal(err)
		}
		u.TrustLevel = trust
	}
	return &Actor{User: u, Via: "web"}
}

// liveItem inserts a live user-posted item directly, bypassing the classifier.
func liveItem(t *testing.T, s *Service, by *Actor) int64 {
	t.Helper()
	seq++
	u := fmt.Sprintf("https://example%d.test/page", seq)
	it, err := s.Q.InsertItem(context.Background(), db.InsertItemParams{Url: u, UrlNorm: u, Domain: "example.test",
		Title: "Thing", Blurb: "A thing worth a look.", CreatedBy: &by.User.ID, Via: "web", Status: "live", Kind: "site", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}

func TestConcurrentVotes(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	poster := newUser(t, s, 1)
	id := liveItem(t, s, poster)
	var voters []*Actor
	for range 20 {
		voters = append(voters, newUser(t, s, 1))
	}
	var wg sync.WaitGroup
	for _, v := range voters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Like, dislike, like again: the final state is one like each.
			for _, val := range []int{1, -1, 1} {
				if _, err := s.Vote(ctx, v, id, val); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	it, err := s.Q.GetItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.LikeCount != 20 || it.DislikeCount != 0 {
		t.Fatalf("likes=%d dislikes=%d, want 20/0", it.LikeCount, it.DislikeCount)
	}
	if it.Score <= 0 {
		t.Fatalf("score not updated: %v", it.Score)
	}
	// Undo one vote.
	if _, err := s.Vote(ctx, voters[0], id, 0); err != nil {
		t.Fatal(err)
	}
	it, _ = s.Q.GetItem(ctx, id)
	if it.LikeCount != 19 {
		t.Fatalf("after undo likes=%d", it.LikeCount)
	}
}

func TestLevelZeroVotesDoNotCount(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	id := liveItem(t, s, newUser(t, s, 1))
	newbie := newUser(t, s, 0)
	if _, err := s.Vote(ctx, newbie, id, 1); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Q.GetItem(ctx, id)
	if it.LikeCount != 0 || it.Score != 0 {
		t.Fatalf("level-0 vote counted: likes=%d score=%v", it.LikeCount, it.Score)
	}
	// Promotion makes the earlier vote count.
	if err := s.SetTrust(ctx, newbie.User.ID, 1); err != nil {
		t.Fatal(err)
	}
	it, _ = s.Q.GetItem(ctx, id)
	if it.LikeCount != 1 {
		t.Fatalf("after promotion likes=%d", it.LikeCount)
	}
}

func TestNoopClassifierHoldsPosts(t *testing.T) {
	s := newTestService(t)
	a := newUser(t, s, 1)
	seq++
	it, err := s.CreateItem(context.Background(), a, NewItem{URL: fmt.Sprintf("https://held%d.test/x", seq),
		Title: "Held", Blurb: "This should wait for a moderator."})
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "pending" {
		t.Fatalf("status %q, want pending", it.Status)
	}
	// Duplicate URL returns 409-style error with the existing id.
	_, err = s.CreateItem(context.Background(), a, NewItem{URL: fmt.Sprintf("http://www.held%d.test/x/", seq),
		Title: "Held", Blurb: "This should wait for a moderator."})
	if e, ok := AsError(err); !ok || e.Code != "duplicate" || e.ItemID != it.ID {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestDeleteAccount(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	poster := newUser(t, s, 1)
	id := liveItem(t, s, poster)
	leaver := newUser(t, s, 1)
	if _, err := s.Vote(ctx, leaver, id, 1); err != nil {
		t.Fatal(err)
	}
	own := liveItem(t, s, leaver)
	if err := s.SaveToList(ctx, leaver, "favorites", id, "great", nil); err != nil {
		t.Fatal(err)
	}
	tok := int64(1)
	if err := s.DeleteAccount(ctx, &Actor{User: leaver.User, TokenID: &tok, Scopes: []string{"read"}}, leaver.User.Username, false); err == nil {
		t.Fatal("token was allowed to delete the account")
	}
	if err := s.DeleteAccount(ctx, leaver, leaver.User.Username, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Q.GetUser(ctx, leaver.User.ID); err == nil {
		t.Fatal("user still exists")
	}
	it, _ := s.Q.GetItem(ctx, id)
	if it.LikeCount != 0 {
		t.Fatalf("vote not recounted: %d", it.LikeCount)
	}
	mine, err := s.Q.GetItem(ctx, own)
	if err != nil || mine.CreatedBy != nil || mine.Status != "live" {
		t.Fatalf("own item should stay, unattributed: %+v %v", mine.CreatedBy, err)
	}
}

func TestPrivateListHidden(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	owner := newUser(t, s, 1)
	other := newUser(t, s, 1)
	l, err := s.CreateList(ctx, owner, ListInput{Title: "Secret stash", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetList(ctx, owner, owner.User.Username, l.Slug); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := s.GetList(ctx, other, owner.User.Username, l.Slug); err != ErrNotFound {
		t.Fatalf("other user: %v", err)
	}
	if _, err := s.GetList(ctx, nil, owner.User.Username, l.Slug); err != ErrNotFound {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestReportsHoldItem(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	id := liveItem(t, s, newUser(t, s, 1))
	for range 3 {
		if err := s.Report(ctx, newUser(t, s, 1), id, "spam", ""); err != nil {
			t.Fatal(err)
		}
	}
	it, _ := s.Q.GetItem(ctx, id)
	if it.Status != "pending" {
		t.Fatalf("status %q after 3 reports", it.Status)
	}
}
