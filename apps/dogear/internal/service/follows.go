package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/safety"
)

// maxFollowsPerUser caps how many people one account can follow: far more
// than anyone reads, low enough to stop scripted mass-following.
const maxFollowsPerUser = 2000

// followingPageSize is how many saves one page of /following shows.
const followingPageSize = 50

// followRules limit follow requests per user, by trust level.
var followRules = map[int16][]safety.Rule{
	0: {{Limit: 30, Per: time.Hour}},
	1: {{Limit: 120, Per: time.Hour}},
}

// followTarget looks up the user to follow as the actor may see them:
// banned accounts do not exist, and shadow-banned ones only for mods.
func (s *Service) followTarget(ctx context.Context, a *Actor, username string) (db.User, error) {
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return db.User{}, err
	}
	if u.ShadowBannedAt != nil && !a.IsMod() {
		return db.User{}, ErrNotFound
	}
	return u, nil
}

// Follow makes the actor follow a user, so that user's public saves show on
// the actor's /following page. Following is refused for yourself, when
// either side has blocked the other, and past maxFollowsPerUser. Following
// someone twice is a no-op.
func (s *Service) Follow(ctx context.Context, a *Actor, username string) error {
	if err := require(a, "vote"); err != nil {
		return err
	}
	u, err := s.followTarget(ctx, a, username)
	if err != nil {
		return err
	}
	if u.ID == a.ID() {
		return errf("invalid", "you cannot follow yourself")
	}
	for _, p := range []db.IsBlockedParams{{BlockerID: a.ID(), BlockedID: u.ID}, {BlockerID: u.ID, BlockedID: a.ID()}} {
		blocked, err := s.Q.IsBlocked(ctx, p)
		if err != nil {
			return fmt.Errorf("blocked: %w", err)
		}
		if blocked {
			return errf("forbidden", "you can't follow @%s", u.Username)
		}
	}
	if err := s.limit("follow:"+strconv.FormatInt(a.ID(), 10), followRules[level(a.User.TrustLevel)]...); err != nil {
		return err
	}
	already, err := s.Q.IsFollowing(ctx, db.IsFollowingParams{FollowerID: a.ID(), FolloweeID: u.ID})
	if err != nil {
		return fmt.Errorf("is following: %w", err)
	}
	if already {
		return nil
	}
	n, err := s.Q.CountFollowing(ctx, a.ID())
	if err != nil {
		return fmt.Errorf("count follows: %w", err)
	}
	if n >= maxFollowsPerUser {
		return errf("invalid", "you can follow at most %d people", maxFollowsPerUser)
	}
	if err := s.Q.FollowUser(ctx, db.FollowUserParams{FollowerID: a.ID(), FolloweeID: u.ID}); err != nil {
		return fmt.Errorf("follow: %w", err)
	}
	return nil
}

// Unfollow stops the actor following a user. It works even when that user
// has since been banned, so a follow can always be undone.
func (s *Service) Unfollow(ctx context.Context, a *Actor, username string) error {
	if err := require(a, "vote"); err != nil {
		return err
	}
	u, err := s.Q.GetUserByUsername(ctx, username)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if err := s.Q.UnfollowUser(ctx, db.UnfollowUserParams{FollowerID: a.ID(), FolloweeID: u.ID}); err != nil {
		return fmt.Errorf("unfollow: %w", err)
	}
	return nil
}

// IsFollowing reports whether the viewer follows userID; false when signed out.
func (s *Service) IsFollowing(ctx context.Context, viewer *Actor, userID int64) bool {
	if viewer == nil {
		return false
	}
	ok, err := s.Q.IsFollowing(ctx, db.IsFollowingParams{FollowerID: viewer.ID(), FolloweeID: userID})
	return err == nil && ok
}

// FollowCounts returns how many people follow a user and how many they
// follow, leaving banned and shadow-banned accounts out of both.
func (s *Service) FollowCounts(ctx context.Context, userID int64) (followers, following int, err error) {
	c, err := s.Q.FollowCounts(ctx, userID)
	if err != nil {
		return 0, 0, fmt.Errorf("follow counts: %w", err)
	}
	return int(c.Followers), int(c.Following), nil
}

// FollowedSave is one entry on /following: an item, the note and public tags
// of the newest person who saved it, and everyone on the page who saved it.
type FollowedSave struct {
	Entry
	// Username is whose note and tags the entry shows (the newest saver).
	Username string `json:"username"`
	// SavedBy lists every followed person who saved the item on this page,
	// newest first, starting with Username.
	SavedBy []string `json:"saved_by"`
}

// FollowingView is one page of saves by the people the viewer follows.
type FollowingView struct {
	Saves []FollowedSave `json:"saves"`
	// Next is the cursor for the following page, empty on the last one.
	Next string `json:"next,omitempty"`
}

// encodeFollowingCursor and decodeFollowingCursor turn a feed position
// (saved_at, saver id, item id) into an opaque "unixnano-user-item" token.
func encodeFollowingCursor(at time.Time, userID, itemID int64) string {
	return strconv.FormatInt(at.UnixNano(), 10) + "-" + strconv.FormatInt(userID, 10) + "-" + strconv.FormatInt(itemID, 10)
}

func decodeFollowingCursor(c string) (*time.Time, int64, int64, error) {
	if c == "" {
		return nil, 0, 0, nil
	}
	parts := strings.Split(c, "-")
	if len(parts) != 3 {
		return nil, 0, 0, errf("invalid", "bad cursor")
	}
	var n [3]int64
	for i, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, 0, 0, errf("invalid", "bad cursor")
		}
		n[i] = v
	}
	t := time.Unix(0, n[0])
	return &t, n[1], n[2], nil
}

// FollowingFeed returns one page of recent saves by the people the viewer
// follows, newest first. Each save follows the same rule as someone's
// public archive: every tag on it is public, the item is live with a
// working link, the saver is not banned or shadow-banned, and nobody on
// either side of a block shows up. Saves of the same item on one page are
// merged into one entry listing everyone who saved it.
func (s *Service) FollowingFeed(ctx context.Context, viewer *Actor, cursor string) (FollowingView, error) {
	if err := require(viewer, "read"); err != nil {
		return FollowingView{}, err
	}
	before, beforeUser, beforeItem, err := decodeFollowingCursor(cursor)
	if err != nil {
		return FollowingView{}, err
	}
	rows, err := s.Q.FollowingSavesPage(ctx, db.FollowingSavesPageParams{ViewerID: viewer.ID(), BeforeAt: before,
		BeforeUser: beforeUser, BeforeItem: beforeItem, Lim: followingPageSize + 1})
	if err != nil {
		return FollowingView{}, fmt.Errorf("following: %w", err)
	}
	var out FollowingView
	if len(rows) > followingPageSize {
		last := rows[followingPageSize-1]
		out.Next = encodeFollowingCursor(last.SavedAt, last.UserID, last.Item.ID)
		rows = rows[:followingPageSize]
	}
	// Keep the newest save of each item; later ones only add their saver.
	var keep []db.FollowingSavesPageRow
	var items []db.Item
	by := map[int64][]string{}
	hidden := map[int64]bool{}
	for _, r := range rows {
		if hidden[r.Item.ID] {
			continue
		}
		if _, seen := by[r.Item.ID]; seen {
			by[r.Item.ID] = append(by[r.Item.ID], r.Username)
			continue
		}
		if ok, err := s.visibleTo(ctx, viewer, r.Item); err != nil || !ok {
			hidden[r.Item.ID] = true
			continue
		}
		by[r.Item.ID] = []string{r.Username}
		keep = append(keep, r)
		items = append(items, r.Item)
	}
	decorated, err := s.decorate(ctx, viewer, items)
	if err != nil {
		return FollowingView{}, err
	}
	// Public tags are looked up once per saver on the page.
	perUser := map[int64][]Item{}
	for i, r := range keep {
		perUser[r.UserID] = append(perUser[r.UserID], decorated[i])
	}
	tags := map[int64]map[int64][]TagRef{}
	for uid, its := range perUser {
		if tags[uid], err = s.tagsFor(ctx, uid, its, false); err != nil {
			return FollowingView{}, err
		}
	}
	out.Saves = make([]FollowedSave, len(keep))
	for i, r := range keep {
		out.Saves[i] = FollowedSave{
			Entry:    Entry{Item: decorated[i], Note: r.Note, AddedAt: r.SavedAt, Tags: tags[r.UserID][r.Item.ID]},
			Username: r.Username, SavedBy: by[r.Item.ID],
		}
	}
	return out, nil
}

// RecentSave is one line of the home page's "recently saved" strip.
type RecentSave struct {
	Username string    `json:"username"`
	ItemID   int64     `json:"item_id"`
	Title    string    `json:"title"`
	SavedAt  time.Time `json:"saved_at"`
}

// recentSaveScan is how many of the newest public saves RecentSaves looks
// through to find limit distinct people and items.
const recentSaveScan = 80

// RecentSaves returns the latest public saves across Dogear for the home
// page: at most one per person and one per item, from trusted accounts
// only, with the same visibility rules as a public archive. Signed-out
// results are cached for a minute since every home page view asks.
func (s *Service) RecentSaves(ctx context.Context, viewer *Actor, limit int) ([]RecentSave, error) {
	var viewerID int64
	if viewer != nil {
		viewerID = viewer.ID()
	}
	key := "recent:" + strconv.Itoa(limit)
	if viewerID == 0 {
		if v, ok := s.recentCache.Get(key); ok {
			return v, nil
		}
	}
	rows, err := s.Q.RecentPublicSaves(ctx, db.RecentPublicSavesParams{ViewerID: viewerID, Lim: recentSaveScan})
	if err != nil {
		return nil, fmt.Errorf("recent saves: %w", err)
	}
	out := []RecentSave{}
	users, items := map[int64]bool{}, map[int64]bool{}
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		if users[r.UserID] || items[r.ID] {
			continue
		}
		users[r.UserID], items[r.ID] = true, true
		out = append(out, RecentSave{Username: r.Username, ItemID: r.ID, Title: r.Title, SavedAt: r.SavedAt})
	}
	if viewerID == 0 {
		s.recentCache.Set(key, out)
	}
	return out, nil
}

// SuggestedPerson is someone worth following, shown on an empty /following.
type SuggestedPerson struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Saves       int    `json:"saves"`
}

// SuggestedPeople returns trusted users with the most public saves that the
// viewer does not follow yet.
func (s *Service) SuggestedPeople(ctx context.Context, viewer *Actor, limit int) ([]SuggestedPerson, error) {
	var viewerID int64
	if viewer != nil {
		viewerID = viewer.ID()
	}
	rows, err := s.Q.SuggestedPeople(ctx, db.SuggestedPeopleParams{ViewerID: viewerID, Lim: int32(limit)})
	if err != nil {
		return nil, fmt.Errorf("suggested people: %w", err)
	}
	out := make([]SuggestedPerson, len(rows))
	for i, r := range rows {
		out[i] = SuggestedPerson{Username: r.Username, Saves: int(r.Saves)}
		if r.DisplayName != nil {
			out[i].DisplayName = *r.DisplayName
		}
	}
	return out, nil
}
