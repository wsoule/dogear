package service

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dogear/internal/db"
)

const (
	maxListsPerUser   = 50
	maxEntriesPerList = 500
)

type List struct {
	ID          int64     `json:"-"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Visibility  string    `json:"visibility"`
	Count       int       `json:"count"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Entry struct {
	Item     Item      `json:"item"`
	Note     string    `json:"note"`
	Position int       `json:"position"`
	AddedAt  time.Time `json:"added_at"`
}

type ListView struct {
	List
	Owner   string  `json:"owner"`
	Entries []Entry `json:"entries"`
	// Indexable is true only for public lists of level 1 owners with 5+ entries.
	Indexable bool `json:"-"`
	IsOwner   bool `json:"-"`
}

func toList(l db.List, count int) List {
	return List{ID: l.ID, Slug: l.Slug, Title: l.Title, Description: l.Description, Visibility: l.Visibility, Count: count, UpdatedAt: l.UpdatedAt}
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify turns a title into a lowercase dash-separated slug.
func Slugify(s string) string {
	s = strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		s = "list"
	}
	return s
}

func validVisibility(v string) bool { return v == "public" || v == "unlisted" || v == "private" }

// uniqueListSlug appends -2, -3, ... until the slug is free for the user.
func uniqueListSlug(ctx context.Context, q *db.Queries, userID int64, title string, except int64) (string, error) {
	base := Slugify(title)
	for i := 1; i < 1000; i++ {
		slug := base
		if i > 1 {
			slug = base + "-" + strconv.Itoa(i)
		}
		l, err := q.GetList(ctx, db.GetListParams{UserID: userID, Slug: slug})
		if notFound(err) || (err == nil && l.ID == except) {
			return slug, nil
		} else if err != nil {
			return "", fmt.Errorf("slug check: %w", err)
		}
	}
	return "", errf("invalid", "too many lists with that title")
}

type ListInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

// CreateList makes a new list for the actor.
func (s *Service) CreateList(ctx context.Context, a *Actor, in ListInput) (List, error) {
	if err := require(a, "post"); err != nil {
		return List{}, err
	}
	title, err := CleanText("title", in.Title, 1, 80)
	if err != nil {
		return List{}, err
	}
	desc, err := CleanText("description", in.Description, 0, 280)
	if err != nil {
		return List{}, err
	}
	if in.Visibility == "" {
		in.Visibility = "public"
	}
	if !validVisibility(in.Visibility) {
		return List{}, errf("invalid", "visibility must be public, unlisted, or private")
	}
	n, err := s.Q.CountUserLists(ctx, a.ID())
	if err != nil {
		return List{}, fmt.Errorf("count lists: %w", err)
	}
	if n >= maxListsPerUser {
		return List{}, errf("invalid", "you can have at most %d lists", maxListsPerUser)
	}
	if err := s.limit("list:"+strconv.FormatInt(a.ID(), 10), listCreateRules...); err != nil {
		return List{}, err
	}
	slug, err := uniqueListSlug(ctx, s.Q, a.ID(), title, 0)
	if err != nil {
		return List{}, err
	}
	l, err := s.Q.CreateList(ctx, db.CreateListParams{UserID: a.ID(), Slug: slug, Title: title, Description: desc, Visibility: in.Visibility})
	if err != nil {
		return List{}, fmt.Errorf("create list: %w", err)
	}
	if l.Visibility == "public" {
		s.markDirty(ctx, a.ID())
	}
	return toList(l, 0), nil
}

type ListPatch struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Visibility  *string `json:"visibility"`
	Position    *int    `json:"position"`
	// ItemOrder reorders entries: item ids in the desired order.
	ItemOrder []int64 `json:"item_order"`
}

// UpdateList renames a list, changes its visibility, or reorders it.
func (s *Service) UpdateList(ctx context.Context, a *Actor, slug string, p ListPatch) (List, error) {
	if err := require(a, "post"); err != nil {
		return List{}, err
	}
	var out List
	err := s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		l, err := q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
		if notFound(err) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("get list: %w", err)
		}
		title, desc, vis, newSlug := l.Title, l.Description, l.Visibility, l.Slug
		if p.Title != nil {
			if title, err = CleanText("title", *p.Title, 1, 80); err != nil {
				return err
			}
			if title != l.Title {
				if newSlug, err = uniqueListSlug(ctx, q, a.ID(), title, l.ID); err != nil {
					return err
				}
			}
		}
		if p.Description != nil {
			if desc, err = CleanText("description", *p.Description, 0, 280); err != nil {
				return err
			}
		}
		if p.Visibility != nil {
			if !validVisibility(*p.Visibility) {
				return errf("invalid", "visibility must be public, unlisted, or private")
			}
			vis = *p.Visibility
		}
		l2, err := q.UpdateList(ctx, db.UpdateListParams{UserID: a.ID(), ID: l.ID, Title: title, Description: desc, Visibility: vis, Slug: newSlug})
		if err != nil {
			return fmt.Errorf("update list: %w", err)
		}
		if p.Position != nil {
			if err := q.SetListOrder(ctx, db.SetListOrderParams{UserID: a.ID(), ID: l.ID, SortOrder: int32(*p.Position)}); err != nil {
				return fmt.Errorf("list order: %w", err)
			}
		}
		for i, id := range p.ItemOrder {
			if err := q.SetEntryPosition(ctx, db.SetEntryPositionParams{ListID: l.ID, ItemID: id, Position: int32(i)}); err != nil {
				return fmt.Errorf("entry order: %w", err)
			}
		}
		n, err := q.CountListEntries(ctx, l.ID)
		if err != nil {
			return fmt.Errorf("count entries: %w", err)
		}
		out = toList(l2, int(n))
		if l.Visibility == "public" || vis == "public" {
			_ = q.MarkDirty(ctx, a.ID())
		}
		return nil
	})
	return out, err
}

// DeleteList deletes a list and its entries.
func (s *Service) DeleteList(ctx context.Context, a *Actor, slug string) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get list: %w", err)
	}
	if err := s.Q.DeleteList(ctx, db.DeleteListParams{UserID: a.ID(), ID: l.ID}); err != nil {
		return fmt.Errorf("delete list: %w", err)
	}
	if l.Visibility == "public" {
		s.markDirty(ctx, a.ID())
	}
	return nil
}

func (s *Service) entryLimit(a *Actor) error {
	if a.TokenID != nil {
		if err := s.limit("entry:tok:"+strconv.FormatInt(*a.TokenID, 10), entryTokenRules...); err != nil {
			return err
		}
	}
	return s.limit("entry:"+strconv.FormatInt(a.ID(), 10), entryRules[level(a.User.TrustLevel)]...)
}

// addEntry inserts or updates one list entry inside a transaction.
func (s *Service) addEntry(ctx context.Context, q *db.Queries, a *Actor, l db.List, itemID int64, note string, position *int) error {
	if err := s.entryLimit(a); err != nil {
		return err
	}
	if _, err := q.GetListEntry(ctx, db.GetListEntryParams{ListID: l.ID, ItemID: itemID}); notFound(err) {
		n, err := q.CountListEntries(ctx, l.ID)
		if err != nil {
			return fmt.Errorf("count entries: %w", err)
		}
		if n >= maxEntriesPerList {
			return errf("invalid", "a list can hold at most %d entries", maxEntriesPerList)
		}
	} else if err != nil {
		return fmt.Errorf("get entry: %w", err)
	}
	var pos *int32
	if position != nil {
		pos = ptr(int32(*position))
	}
	if err := q.UpsertListEntry(ctx, db.UpsertListEntryParams{ListID: l.ID, ItemID: itemID, Note: note, Position: pos}); err != nil {
		return fmt.Errorf("upsert entry: %w", err)
	}
	if err := q.TouchList(ctx, l.ID); err != nil {
		return fmt.Errorf("touch list: %w", err)
	}
	if l.Visibility == "public" {
		_ = q.MarkDirty(ctx, a.ID())
	}
	return nil
}

// SaveToList adds an existing item to one of the actor's lists with a note,
// or updates the note and position when it is already there. Saving is not
// a vote and never changes ranking.
func (s *Service) SaveToList(ctx context.Context, a *Actor, slug string, itemID int64, note string, position *int) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	note, err := CleanText("note", note, 0, 280)
	if err != nil {
		return err
	}
	it, err := s.Q.GetItem(ctx, itemID)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get item: %w", err)
	}
	if ok, err := s.visibleTo(ctx, a, it); err != nil {
		return err
	} else if !ok {
		return ErrNotFound
	}
	return s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		l, err := q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
		if notFound(err) {
			return errf("not_found", "you have no list %q", slug)
		} else if err != nil {
			return fmt.Errorf("get list: %w", err)
		}
		return s.addEntry(ctx, q, a, l, itemID, note, position)
	})
}

// RemoveFromList removes an item from one of the actor's lists.
func (s *Service) RemoveFromList(ctx context.Context, a *Actor, slug string, itemID int64) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	if err := s.entryLimit(a); err != nil {
		return err
	}
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get list: %w", err)
	}
	if err := s.Q.DeleteListEntry(ctx, db.DeleteListEntryParams{ListID: l.ID, ItemID: itemID}); err != nil {
		return fmt.Errorf("delete entry: %w", err)
	}
	if l.Visibility == "public" {
		s.markDirty(ctx, a.ID())
	}
	return nil
}

// MyLists returns all of the actor's lists, including private ones.
func (s *Service) MyLists(ctx context.Context, a *Actor) ([]List, error) {
	if err := require(a, "read"); err != nil {
		return nil, err
	}
	return s.userLists(ctx, a.ID(), true)
}

func (s *Service) userLists(ctx context.Context, userID int64, all bool) ([]List, error) {
	rows, err := s.Q.UserLists(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("user lists: %w", err)
	}
	out := []List{}
	for _, r := range rows {
		if !all && r.Visibility != "public" {
			continue
		}
		out = append(out, List{ID: r.ID, Slug: r.Slug, Title: r.Title, Description: r.Description,
			Visibility: r.Visibility, Count: int(r.EntryCount), UpdatedAt: r.UpdatedAt})
	}
	return out, nil
}

// SavedIn returns the slugs of the actor's lists that contain an item.
func (s *Service) SavedIn(ctx context.Context, a *Actor, itemID int64) ([]string, error) {
	if a == nil {
		return nil, nil
	}
	return s.Q.ItemSavedIn(ctx, db.ItemSavedInParams{UserID: a.ID(), ItemID: itemID})
}

// GetList returns one list as seen by viewer. Private lists are not found for
// everyone but the owner; pending, removed, and dead items stay in the owner's
// view with a status label and are hidden from everyone else.
func (s *Service) GetList(ctx context.Context, viewer *Actor, username, slug string) (ListView, error) {
	u, err := s.Q.GetUserByUsername(ctx, username)
	if notFound(err) || (err == nil && u.BannedAt != nil) {
		return ListView{}, ErrNotFound
	} else if err != nil {
		return ListView{}, fmt.Errorf("get user: %w", err)
	}
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: u.ID, Slug: slug})
	if notFound(err) {
		return ListView{}, ErrNotFound
	} else if err != nil {
		return ListView{}, fmt.Errorf("get list: %w", err)
	}
	owner := viewer != nil && viewer.ID() == u.ID
	if l.Visibility == "private" && !owner {
		return ListView{}, ErrNotFound
	}
	if owner && viewer.Scopes != nil && !viewer.Can("read") {
		return ListView{}, ErrNotFound
	}
	rows, err := s.Q.ListEntries(ctx, l.ID)
	if err != nil {
		return ListView{}, fmt.Errorf("entries: %w", err)
	}
	var items []db.Item
	var keep []db.ListEntriesRow
	for _, r := range rows {
		it := db.Item{ID: r.ID, Url: r.Url, UrlNorm: r.UrlNorm, Domain: r.Domain, Title: r.Title, Blurb: r.Blurb,
			CreatedBy: r.CreatedBy, CreatedViaToken: r.CreatedViaToken, Via: r.Via, Status: r.Status,
			LinkStatus: r.LinkStatus, LinkFailCount: r.LinkFailCount, LinkCheckedAt: r.LinkCheckedAt,
			GithubStars: r.GithubStars, ListCount: r.ListCount, PriorScore: r.PriorScore, LikeCount: r.LikeCount,
			DislikeCount: r.DislikeCount, Score: r.Score, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Kind: r.Kind, Creator: r.Creator, Flags: r.Flags, RemovedAt: r.RemovedAt}
		if !owner {
			if it.Status != "live" || it.LinkStatus == "dead" {
				continue
			}
			if ok, err := s.visibleTo(ctx, viewer, it); err != nil || !ok {
				continue
			}
		}
		items = append(items, it)
		keep = append(keep, r)
	}
	decorated, err := s.decorate(ctx, viewer, items)
	if err != nil {
		return ListView{}, err
	}
	lv := ListView{List: toList(l, len(keep)), Owner: u.Username, IsOwner: owner, Entries: []Entry{}}
	for i, r := range keep {
		lv.Entries = append(lv.Entries, Entry{Item: decorated[i], Note: r.Note, Position: int(r.Position), AddedAt: r.AddedAt})
	}
	lv.Indexable = l.Visibility == "public" && u.TrustLevel >= 1 && len(lv.Entries) >= 5 && u.ShadowBannedAt == nil
	return lv, nil
}

func (s *Service) markDirty(ctx context.Context, userID int64) {
	if err := s.Q.MarkDirty(ctx, userID); err != nil {
		s.Log.Warn("mark dirty", "user", userID, "err", err)
	}
}
