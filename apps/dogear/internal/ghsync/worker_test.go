package ghsync

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeStore mimics the github_sync table for one or more users.
type fakeStore struct {
	mu       sync.Mutex
	now      func() time.Time
	rows     map[int64]*fakeRow
	profiles map[int64]Profile
}

type fakeRow struct {
	Row
	status       string
	dirtySince   *time.Time
	lastSyncedAt *time.Time
}

func (s *fakeStore) markDirty(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[userID]
	if r.dirtySince == nil {
		t := s.now()
		r.dirtySince = &t
	}
}

func (s *fakeStore) DirtyRows(_ context.Context, olderThan time.Time) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Row
	for _, r := range s.rows {
		if r.status != "active" || r.dirtySince == nil || !r.dirtySince.Before(olderThan) {
			continue
		}
		if r.lastSyncedAt != nil && !r.lastSyncedAt.Before(s.now().Add(-5*time.Minute)) {
			continue
		}
		out = append(out, r.Row)
	}
	return out, nil
}

func (s *fakeStore) LoadProfile(_ context.Context, userID int64, _ bool) (Profile, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.profiles[userID]
	n := len(p.Posted)
	for _, l := range p.Lists {
		n += len(l.Entries)
	}
	return p, n, nil
}

func (s *fakeStore) MarkSynced(_ context.Context, userID int64, sha []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[userID]
	t := s.now()
	r.ContentSHA256 = append([]byte(nil), sha...)
	r.lastSyncedAt = &t
	r.dirtySince = nil
	return nil
}

func (s *fakeStore) MarkPaused(_ context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[userID].status = "paused"
	return nil
}

// workerFixture builds a worker with a controllable clock against a fake GitHub.
func workerFixture(t *testing.T, gh *fakeGitHub) (*Worker, *fakeStore, *time.Time) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	st := &fakeStore{
		now: now,
		rows: map[int64]*fakeRow{1: {
			Row:    Row{UserID: 1, InstallationID: 7, RepoFullName: "ada/awesome-ada"},
			status: "active",
		}},
		profiles: map[int64]Profile{1: {Username: "ada", Lists: []List{{Title: "Tools", Entries: []Entry{{Title: "Go", URL: "https://go.dev"}}}}}},
	}
	w := &Worker{Store: st, App: newTestApp(t, gh), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: now}
	return w, st, &clock
}

func TestSyncOnceCommitsOnceForTwoDirtyMarks(t *testing.T) {
	gh := &fakeGitHub{t: t}
	w, st, clock := workerFixture(t, gh)
	ctx := context.Background()

	st.markDirty(1)
	*clock = clock.Add(time.Minute)
	st.markDirty(1)
	// Not dirty long enough yet.
	if err := w.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(gh.puts) != 0 {
		t.Fatalf("committed too early: %d", len(gh.puts))
	}
	*clock = clock.Add(5 * time.Minute)
	for range 2 {
		if err := w.SyncOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(gh.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(gh.puts))
	}
	if msg := gh.puts[0]["message"]; msg != "Update list (1 item)" {
		t.Errorf("message %q", msg)
	}
	r := st.rows[1]
	want := sha256.Sum256(Render(st.profiles[1]))
	if string(r.ContentSHA256) != string(want[:]) || r.dirtySince != nil || r.lastSyncedAt == nil {
		t.Errorf("row not marked synced: %+v", r)
	}
}

func TestSyncOnceSkipsUnchangedContent(t *testing.T) {
	gh := &fakeGitHub{t: t}
	w, st, clock := workerFixture(t, gh)
	sum := sha256.Sum256(Render(st.profiles[1]))
	st.rows[1].ContentSHA256 = sum[:]
	st.markDirty(1)
	*clock = clock.Add(6 * time.Minute)
	if err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gh.puts) != 0 || gh.tokenCalls.Load() != 0 {
		t.Errorf("expected no GitHub calls, puts=%d tokens=%d", len(gh.puts), gh.tokenCalls.Load())
	}
	if st.rows[1].dirtySince != nil || st.rows[1].lastSyncedAt == nil {
		t.Error("row should still be marked synced")
	}
}

func TestSyncOncePausesWhenRepoGone(t *testing.T) {
	gh := &fakeGitHub{t: t, repoGone: true}
	w, st, clock := workerFixture(t, gh)
	st.markDirty(1)
	*clock = clock.Add(6 * time.Minute)
	if err := w.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.rows[1].status != "paused" {
		t.Errorf("status = %q, want paused", st.rows[1].status)
	}
	if st.rows[1].lastSyncedAt != nil {
		t.Error("paused row should not be marked synced")
	}
}
