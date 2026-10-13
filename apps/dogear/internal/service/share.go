package service

import (
	"context"
	"fmt"

	"dogear/internal/db"
	"dogear/internal/safety"
)

// SharedArchive is a user's archive exactly as a signed-out visitor sees it,
// for preview images, RSS, and other share surfaces that must never show
// more than the public page.
type SharedArchive struct {
	User    db.User
	Entries []Entry // newest first, at most one archive page
	Count   int     // every save the public can see, not just Entries
}

// SharedTag is one public tag as a signed-out visitor sees it, with its
// owner so share surfaces can show a display name.
type SharedTag struct {
	User db.User
	ListView
}

// publicOwner loads a user for share surfaces: banned and shadow-banned
// users are not found, matching what the profile shows a visitor.
func (s *Service) publicOwner(ctx context.Context, username string) (db.User, error) {
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return db.User{}, err
	}
	if u.ShadowBannedAt != nil {
		return db.User{}, ErrNotFound
	}
	return u, nil
}

// PublicArchive returns the newest page of a user's archive with the public
// view's visibility rules (saves in any non-public tag are hidden) and the
// total public count.
func (s *Service) PublicArchive(ctx context.Context, username string) (SharedArchive, error) {
	u, err := s.publicOwner(ctx, username)
	if err != nil {
		return SharedArchive{}, err
	}
	av, err := s.Archive(ctx, nil, u.ID, "")
	if err != nil {
		return SharedArchive{}, err
	}
	n, err := s.Q.CountPublicSaves(ctx, u.ID)
	if err != nil {
		return SharedArchive{}, fmt.Errorf("count public saves: %w", err)
	}
	return SharedArchive{User: u, Entries: av.Entries, Count: int(n)}, nil
}

// PublicTag returns a tag as a signed-out visitor sees it. Only public tags
// are found: unlisted tags are link-only, so they get no preview image,
// feed, or embed that could spread them further.
func (s *Service) PublicTag(ctx context.Context, username, slug string) (SharedTag, error) {
	u, err := s.publicOwner(ctx, username)
	if err != nil {
		return SharedTag{}, err
	}
	lv, err := s.GetList(ctx, nil, username, slug)
	if err != nil {
		return SharedTag{}, err
	}
	if lv.Visibility != "public" {
		return SharedTag{}, ErrNotFound
	}
	return SharedTag{User: u, ListView: lv}, nil
}

// ItemForURL finds the item already on Dogear for a shared link, if the
// viewer may see it. The share target uses it to open the item page (to
// dogear or tag it) instead of the post form.
func (s *Service) ItemForURL(ctx context.Context, viewer *Actor, raw string) (int64, bool) {
	norm, err := safety.NormalizeURL(raw)
	if err != nil {
		return 0, false
	}
	it, err := s.Q.GetItemByNorm(ctx, norm)
	if err != nil {
		return 0, false
	}
	if ok, err := s.visibleTo(ctx, viewer, it); err != nil || !ok {
		return 0, false
	}
	return it.ID, true
}
