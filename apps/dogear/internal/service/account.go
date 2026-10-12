package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dogear/internal/db"
)

// DeleteAccount deletes the user and everything tied to them in one
// transaction, then recounts every item they voted on. Their posts stay,
// unattributed, unless deletePosts is set. Only session-authenticated web
// requests may call it; bearer tokens are refused.
func (s *Service) DeleteAccount(ctx context.Context, a *Actor, confirm string, deletePosts bool) error {
	if a == nil {
		return ErrUnauthorized
	}
	if a.TokenID != nil || a.Scopes != nil {
		return errf("forbidden", "accounts can only be deleted from the website")
	}
	if !strings.EqualFold(strings.TrimSpace(confirm), a.User.Username) {
		return errf("invalid", "type your username to confirm")
	}
	var voted []int64
	var sync *db.GithubSync
	err := s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		var err error
		if voted, err = q.ItemsVotedBy(ctx, a.ID()); err != nil {
			return fmt.Errorf("voted items: %w", err)
		}
		if row, err := q.SyncRow(ctx, a.ID()); err == nil {
			sync = &row
		}
		if deletePosts {
			if err := q.RemoveUserItems(ctx, ptr(a.ID())); err != nil {
				return fmt.Errorf("remove posts: %w", err)
			}
		}
		// Sessions, tokens, identities, votes, lists, entries, reports, the
		// sync row, invites, and blocks cascade from the user row; items are
		// SET NULL so they stay unattributed.
		if err := q.DeleteUser(ctx, a.ID()); err != nil {
			return fmt.Errorf("delete user: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error { return recountItems(ctx, q, voted) }); err != nil {
		s.Log.Error("recount after delete", "err", err)
	}
	if sync != nil && s.UninstallApp != nil {
		if err := s.UninstallApp(ctx, sync.InstallationID); err != nil {
			s.Log.Warn("uninstall app", "installation", sync.InstallationID, "err", err)
		}
	}
	return nil
}

// Export returns everything the site holds about the user.
func (s *Service) Export(ctx context.Context, a *Actor) (map[string]any, error) {
	if err := require(a, "read"); err != nil {
		return nil, err
	}
	u := a.User
	ids, err := s.Q.ListIdentities(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	items, err := s.Q.AllItemsByUser(ctx, &u.ID)
	if err != nil {
		return nil, err
	}
	votes, err := s.Q.UserVotesExport(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	tokens, err := s.Q.ListTokens(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	lists, err := s.userLists(ctx, u.ID, true)
	if err != nil {
		return nil, err
	}
	var listOut []map[string]any
	for _, l := range lists {
		lv, err := s.GetList(ctx, a, u.Username, l.Slug)
		if err != nil {
			return nil, err
		}
		var entries []map[string]any
		for _, e := range lv.Entries {
			entries = append(entries, map[string]any{"item_id": e.Item.ID, "url": e.Item.URL, "title": e.Item.Title, "note": e.Note, "position": e.Position, "added_at": e.AddedAt})
		}
		listOut = append(listOut, map[string]any{"slug": l.Slug, "title": l.Title, "description": l.Description, "visibility": l.Visibility, "entries": entries})
	}
	var identOut, itemOut, voteOut, tokOut []map[string]any
	for _, i := range ids {
		identOut = append(identOut, map[string]any{"provider": i.Provider, "provider_user_id": i.ProviderUserID, "linked_at": i.CreatedAt})
	}
	for _, it := range items {
		itemOut = append(itemOut, map[string]any{"id": it.ID, "url": it.Url, "title": it.Title, "blurb": it.Blurb, "kind": it.Kind,
			"creator": it.Creator, "status": it.Status, "via": it.Via, "created_at": it.CreatedAt})
	}
	for _, v := range votes {
		voteOut = append(voteOut, map[string]any{"item_id": v.ItemID, "url": v.Url, "title": v.Title, "value": v.Value, "counted": v.Counted, "via": v.Via, "created_at": v.CreatedAt})
	}
	saves, err := s.Q.UserSaves(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	saveOut := []map[string]any{}
	for _, sv := range saves {
		saveOut = append(saveOut, map[string]any{"item_id": sv.ID, "url": sv.Url, "title": sv.Title, "note": sv.Note, "saved_at": sv.SavedAt})
	}
	for _, t := range tokens {
		tokOut = append(tokOut, map[string]any{"name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes, "created_at": t.CreatedAt, "last_used_at": t.LastUsedAt})
	}
	return map[string]any{
		"exported_at": time.Now().UTC(),
		"profile": map[string]any{"username": u.Username, "display_name": u.DisplayName, "bio": u.Bio, "website": u.Website,
			"avatar_url": u.AvatarUrl, "role": u.Role, "trust_level": u.TrustLevel, "created_at": u.CreatedAt},
		"identities": identOut, "items": itemOut, "saves": saveOut, "lists": listOut, "votes": voteOut, "tokens": tokOut,
	}, nil
}

// Block hides a user's items from the actor's feeds, search, and item pages.
func (s *Service) Block(ctx context.Context, a *Actor, username string, block bool) error {
	if err := require(a, "vote"); err != nil {
		return err
	}
	u, err := s.Q.GetUserByUsername(ctx, username)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if u.ID == a.ID() {
		return errf("invalid", "you cannot block yourself")
	}
	if block {
		return s.Q.BlockUser(ctx, db.BlockUserParams{BlockerID: a.ID(), BlockedID: u.ID})
	}
	return s.Q.UnblockUser(ctx, db.UnblockUserParams{BlockerID: a.ID(), BlockedID: u.ID})
}
