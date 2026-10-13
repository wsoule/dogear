package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/ghsync"
)

// SyncStore adapts the database to ghsync.Store for the README sync worker.
type SyncStore struct{ S *Service }

func (st SyncStore) DirtyRows(ctx context.Context, olderThan time.Time) ([]ghsync.Row, error) {
	rows, err := st.S.Q.DirtySyncRows(ctx, &olderThan)
	if err != nil {
		return nil, fmt.Errorf("dirty rows: %w", err)
	}
	out := make([]ghsync.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, ghsync.Row{UserID: r.UserID, InstallationID: r.InstallationID, RepoFullName: r.RepoFullName,
			IncludePosted: r.IncludePosted, ContentSHA256: r.ContentSha256})
	}
	return out, nil
}

// LoadProfile builds the README input: public lists with live, non-dead
// items only, plus (optionally) posts not already in a list.
func (st SyncStore) LoadProfile(ctx context.Context, userID int64, includePosted bool) (ghsync.Profile, int, error) {
	s := st.S
	u, err := s.Q.GetUser(ctx, userID)
	if err != nil {
		return ghsync.Profile{}, 0, fmt.Errorf("get user: %w", err)
	}
	p := ghsync.Profile{Username: u.Username, Bio: u.Bio}
	if u.DisplayName != nil {
		p.DisplayName = *u.DisplayName
	}
	lists, err := s.userLists(ctx, userID, false)
	if err != nil {
		return p, 0, err
	}
	count := 0
	inList := map[int64]bool{}
	for _, l := range lists {
		rows, err := s.Q.ListEntries(ctx, l.ID)
		if err != nil {
			return p, 0, fmt.Errorf("entries: %w", err)
		}
		gl := ghsync.List{Title: l.Title, Slug: l.Slug, Description: l.Description}
		for _, r := range rows {
			it := r.Item
			if !shareable(it.Status) || it.LinkStatus == "dead" {
				continue
			}
			gl.Entries = append(gl.Entries, ghsync.Entry{Title: it.Title, URL: it.Url, Note: r.Note, Blurb: it.Blurb})
			inList[it.ID] = true
			count++
		}
		p.Lists = append(p.Lists, gl)
	}
	if includePosted {
		items, err := s.Q.ItemsByUser(ctx, db.ItemsByUserParams{CreatedBy: &userID, Limit: 1000})
		if err != nil {
			return p, 0, fmt.Errorf("posts: %w", err)
		}
		for _, it := range items {
			if it.Status != "live" || it.LinkStatus == "dead" || inList[it.ID] {
				continue
			}
			p.Posted = append(p.Posted, ghsync.Entry{Title: it.Title, URL: it.Url, Blurb: it.Blurb})
			count++
		}
	}
	return p, count, nil
}

func (st SyncStore) MarkSynced(ctx context.Context, userID int64, sha []byte) error {
	return st.S.Q.MarkSynced(ctx, db.MarkSyncedParams{UserID: userID, ContentSha256: sha})
}

func (st SyncStore) MarkPaused(ctx context.Context, userID int64) error {
	return st.S.Q.SetSyncStatus(ctx, db.SetSyncStatusParams{UserID: userID, Status: "paused"})
}

// ConnectRepo finishes the GitHub App install flow: the installing account
// must be the user's linked GitHub identity and the installation must cover
// exactly one repository.
func (s *Service) ConnectRepo(ctx context.Context, u db.User, app *ghsync.App, installationID int64) (string, error) {
	gh, err := s.Q.GitHubIdentity(ctx, u.ID)
	if notFound(err) {
		return "", errf("invalid", "link your GitHub account first")
	} else if err != nil {
		return "", fmt.Errorf("github identity: %w", err)
	}
	_, accountID, err := app.InstallationAccount(ctx, installationID)
	if err != nil {
		return "", fmt.Errorf("installation account: %w", err)
	}
	if fmt.Sprint(accountID) != gh.ProviderUserID {
		return "", errf("forbidden", "the app was installed on a different GitHub account than the one linked here")
	}
	repos, err := app.InstallationRepos(ctx, installationID)
	if err != nil {
		return "", fmt.Errorf("installation repos: %w", err)
	}
	if len(repos) != 1 {
		return "", errf("invalid", "install the app on exactly one repository (Only select repositories)")
	}
	if err := s.Q.UpsertSyncRow(ctx, db.UpsertSyncRowParams{UserID: u.ID, InstallationID: installationID, RepoFullName: repos[0]}); err != nil {
		return "", fmt.Errorf("sync row: %w", err)
	}
	return repos[0], nil
}

func (s *Service) SyncStatus(ctx context.Context, userID int64) (*db.GithubSync, error) {
	r, err := s.Q.SyncRow(ctx, userID)
	if notFound(err) {
		return nil, nil
	}
	return &r, err
}

func (s *Service) DisconnectRepo(ctx context.Context, userID int64) error {
	return s.Q.DeleteSyncRow(ctx, userID)
}

func (s *Service) SetIncludePosted(ctx context.Context, userID int64, on bool) error {
	return s.Q.SetIncludePosted(ctx, db.SetIncludePostedParams{UserID: userID, IncludePosted: on})
}

// HandleWebhook revokes sync rows when an installation is deleted or the
// synced repo is removed from it.
func (s *Service) HandleWebhook(ctx context.Context, ev ghsync.WebhookEvent) error {
	if ev.Kind != ghsync.KindRevoke {
		return nil
	}
	if len(ev.RemovedRepos) == 0 {
		return s.Q.RevokeSyncByInstallation(ctx, ev.InstallationID)
	}
	rows, err := s.Q.SyncRowByInstallation(ctx, ev.InstallationID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if slices.ContainsFunc(ev.RemovedRepos, func(repo string) bool { return strings.EqualFold(repo, r.RepoFullName) }) {
			if err := s.Q.SetSyncStatus(ctx, db.SetSyncStatusParams{UserID: r.UserID, Status: "revoked"}); err != nil {
				return err
			}
		}
	}
	return nil
}
