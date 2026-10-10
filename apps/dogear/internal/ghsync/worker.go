package ghsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// syncDelay is how long a row must stay dirty before it is synced, which also
// caps commits at one per user per 5 minutes.
const syncDelay = 5 * time.Minute

// Row is one github_sync row that needs syncing.
type Row struct {
	UserID, InstallationID int64
	RepoFullName           string
	IncludePosted          bool
	ContentSHA256          []byte
}

// Store is the database access the worker needs.
type Store interface {
	// DirtyRows returns active rows with dirty_since < olderThan and
	// (last_synced_at is null or < now-5m).
	DirtyRows(ctx context.Context, olderThan time.Time) ([]Row, error)
	// LoadProfile returns the README input and its total item count.
	LoadProfile(ctx context.Context, userID int64, includePosted bool) (Profile, int, error)
	// MarkSynced sets content_sha256, last_synced_at=now, dirty_since=NULL.
	MarkSynced(ctx context.Context, userID int64, sha []byte) error
	// MarkPaused sets status='paused'.
	MarkPaused(ctx context.Context, userID int64) error
}

// Worker periodically pushes dirty users' READMEs to GitHub.
type Worker struct {
	Store Store
	App   *App
	Log   *slog.Logger
	Now   func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// SyncOnce syncs every row that has been dirty for over 5 minutes. A failure
// for one user is logged and does not stop the rest of the batch; only a
// failure to list dirty rows is returned.
func (w *Worker) SyncOnce(ctx context.Context) error {
	rows, err := w.Store.DirtyRows(ctx, w.now().Add(-syncDelay))
	if err != nil {
		return fmt.Errorf("ghsync: list dirty rows: %w", err)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.syncRow(ctx, row); err != nil {
			w.log().Error("ghsync: sync failed", "user_id", row.UserID, "repo", row.RepoFullName, "err", err)
		}
	}
	return nil
}

// syncRow renders one user's README and commits it unless the content hash
// matches the last one written. A gone repo pauses the row.
func (w *Worker) syncRow(ctx context.Context, row Row) error {
	profile, count, err := w.Store.LoadProfile(ctx, row.UserID, row.IncludePosted)
	if err != nil {
		return fmt.Errorf("load profile: %w", err)
	}
	readme := Render(profile)
	sum := sha256.Sum256(readme)
	if !bytes.Equal(sum[:], row.ContentSHA256) {
		noun := "items"
		if count == 1 {
			noun = "item"
		}
		msg := fmt.Sprintf("Update list (%d %s)", count, noun)
		if err := w.App.PutReadme(ctx, row.InstallationID, row.RepoFullName, readme, msg); err != nil {
			if errors.Is(err, ErrRepoGone) {
				w.log().Warn("ghsync: repo gone, pausing", "user_id", row.UserID, "repo", row.RepoFullName, "err", err)
				return w.Store.MarkPaused(ctx, row.UserID)
			}
			return fmt.Errorf("put readme: %w", err)
		}
	}
	return w.Store.MarkSynced(ctx, row.UserID, sum[:])
}

// Run calls SyncOnce immediately and then every minute until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := w.SyncOnce(ctx); err != nil && ctx.Err() == nil {
			w.log().Error("ghsync: sync batch failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
