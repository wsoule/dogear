package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
)

// maxSavesPerUser caps an archive: far beyond a lifetime of saving by hand,
// low enough to stop scripted abuse.
const maxSavesPerUser = 10000

// archivePageSize is how many saves one page of /u/{user} shows.
const archivePageSize = 50

// TagRef is one of the owner's tags on an archive entry.
type TagRef struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Visibility string `json:"visibility"`
}

// saveItem upserts the actor's save of an item inside a transaction. A nil
// note keeps whatever note the save already has. It enforces the archive cap
// on new saves only.
func (s *Service) saveItem(ctx context.Context, q *db.Queries, a *Actor, itemID int64, note *string) error {
	if _, err := q.GetSave(ctx, db.GetSaveParams{UserID: a.ID(), ItemID: itemID}); notFound(err) {
		n, err := q.CountSaves(ctx, a.ID())
		if err != nil {
			return fmt.Errorf("count saves: %w", err)
		}
		if n >= maxSavesPerUser {
			return errf("invalid", "your archive can hold at most %d things", maxSavesPerUser)
		}
	} else if err != nil {
		return fmt.Errorf("get save: %w", err)
	}
	if _, err := q.UpsertSave(ctx, db.UpsertSaveParams{UserID: a.ID(), ItemID: itemID, Note: note}); err != nil {
		return fmt.Errorf("upsert save: %w", err)
	}
	if note != nil {
		// Every tag holding the item shows this note, so loggit links on
		// those tags have something new to sync.
		if err := q.MarkTagLinksDirtyForItem(ctx, db.MarkTagLinksDirtyForItemParams{UserID: a.ID(), ItemID: itemID}); err != nil {
			return fmt.Errorf("mark links: %w", err)
		}
	}
	_ = q.MarkDirty(ctx, a.ID())
	return nil
}

// checkSavable loads an item and confirms the actor may see it.
func (s *Service) checkSavable(ctx context.Context, a *Actor, itemID int64) error {
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
	return nil
}

// SaveItem adds an item to the actor's archive, or updates its note when it
// is already there. A nil note leaves an existing note alone; an empty one
// clears it. Saving is not a vote and never changes ranking.
func (s *Service) SaveItem(ctx context.Context, a *Actor, itemID int64, note *string) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	if note != nil {
		n, err := CleanText("note", *note, 0, 280)
		if err != nil {
			return err
		}
		note = &n
	}
	if err := s.checkSavable(ctx, a, itemID); err != nil {
		return err
	}
	if err := s.entryLimit(a); err != nil {
		return err
	}
	return s.inTxQ(ctx, func(q *db.Queries) error { return s.saveItem(ctx, q, a, itemID, note) })
}

// Unsave removes an item from the actor's archive and from every one of
// their tags, since a tag entry never exists without a save.
func (s *Service) Unsave(ctx context.Context, a *Actor, itemID int64) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	if err := s.entryLimit(a); err != nil {
		return err
	}
	return s.inTxQ(ctx, func(q *db.Queries) error {
		lists, err := q.UntagEverywhere(ctx, db.UntagEverywhereParams{UserID: a.ID(), ItemID: itemID})
		if err != nil {
			return fmt.Errorf("untag: %w", err)
		}
		for _, id := range lists {
			_ = q.MarkListLinkDirty(ctx, id)
		}
		if err := q.DeleteSave(ctx, db.DeleteSaveParams{UserID: a.ID(), ItemID: itemID}); err != nil {
			return fmt.Errorf("delete save: %w", err)
		}
		_ = q.MarkDirty(ctx, a.ID())
		return nil
	})
}

// ToggleDogear saves an item to the actor's archive, or unsaves it (and
// untags it everywhere) when it is already saved. It returns whether the
// item is saved afterwards.
func (s *Service) ToggleDogear(ctx context.Context, a *Actor, itemID int64) (bool, error) {
	if err := require(a, "post"); err != nil {
		return false, err
	}
	if _, err := s.Q.GetSave(ctx, db.GetSaveParams{UserID: a.ID(), ItemID: itemID}); err == nil {
		return false, s.Unsave(ctx, a, itemID)
	} else if !notFound(err) {
		return false, fmt.Errorf("get save: %w", err)
	}
	return true, s.SaveItem(ctx, a, itemID, nil)
}

// SavedNote returns the actor's note on an item and whether it is saved.
func (s *Service) SavedNote(ctx context.Context, a *Actor, itemID int64) (string, bool) {
	if a == nil {
		return "", false
	}
	sv, err := s.Q.GetSave(ctx, db.GetSaveParams{UserID: a.ID(), ItemID: itemID})
	if err != nil {
		return "", false
	}
	return sv.Note, true
}

// ArchiveView is one page of a user's archive as a viewer sees it.
type ArchiveView struct {
	Entries []Entry `json:"entries"`
	// Next is the cursor for the following page, empty on the last one.
	Next string `json:"next,omitempty"`
}

// encodeArchiveCursor and decodeArchiveCursor turn an archive position (saved_at, item id)
// into an opaque "unixnano-id" token for ?before=.
func encodeArchiveCursor(at time.Time, id int64) string {
	return strconv.FormatInt(at.UnixNano(), 10) + "-" + strconv.FormatInt(id, 10)
}

func decodeArchiveCursor(c string) (*time.Time, int64, error) {
	if c == "" {
		return nil, 0, nil
	}
	ns, id, ok := strings.Cut(c, "-")
	n, err1 := strconv.ParseInt(ns, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return nil, 0, errf("invalid", "bad cursor")
	}
	t := time.Unix(0, n)
	return &t, i, nil
}

// Archive returns one page of a user's saves, newest first. Owners see every
// save; everyone else sees only saves whose tags are all public, and only
// live items with working links.
func (s *Service) Archive(ctx context.Context, viewer *Actor, userID int64, cursor string) (ArchiveView, error) {
	before, beforeID, err := decodeArchiveCursor(cursor)
	if err != nil {
		return ArchiveView{}, err
	}
	// A token without the read scope gets the public view, as GetList does.
	owner := viewer != nil && viewer.ID() == userID && viewer.Can("read")
	rows, err := s.Q.ArchivePage(ctx, db.ArchivePageParams{UserID: userID, BeforeAt: before, BeforeID: beforeID,
		PublicOnly: !owner, Lim: archivePageSize + 1})
	if err != nil {
		return ArchiveView{}, fmt.Errorf("archive: %w", err)
	}
	var out ArchiveView
	if len(rows) > archivePageSize {
		last := rows[archivePageSize-1]
		out.Next = encodeArchiveCursor(last.SavedAt, last.ID)
		rows = rows[:archivePageSize]
	}
	var items []db.Item
	var keep []db.ArchivePageRow
	for _, r := range rows {
		it := db.Item{ID: r.ID, Url: r.Url, UrlNorm: r.UrlNorm, Domain: r.Domain, Title: r.Title, Blurb: r.Blurb,
			CreatedBy: r.CreatedBy, CreatedViaToken: r.CreatedViaToken, Via: r.Via, Status: r.Status,
			LinkStatus: r.LinkStatus, LinkFailCount: r.LinkFailCount, LinkCheckedAt: r.LinkCheckedAt,
			GithubStars: r.GithubStars, ListCount: r.ListCount, PriorScore: r.PriorScore, LikeCount: r.LikeCount,
			DislikeCount: r.DislikeCount, Score: r.Score, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Kind: r.Kind, Creator: r.Creator, Flags: r.Flags, RemovedAt: r.RemovedAt,
			ImageUrl: r.ImageUrl, ImageState: r.ImageState, ImageCheckedAt: r.ImageCheckedAt}
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
		return ArchiveView{}, err
	}
	tags, err := s.tagsFor(ctx, userID, decorated, owner)
	if err != nil {
		return ArchiveView{}, err
	}
	out.Entries = make([]Entry, len(keep))
	for i, r := range keep {
		out.Entries[i] = Entry{Item: decorated[i], Note: r.Note, AddedAt: r.SavedAt, Tags: tags[r.ID]}
	}
	return out, nil
}

// tagsFor maps item id to the user's tags on it. Non-owners see public tags
// only (the archive already hides saves in any non-public tag).
func (s *Service) tagsFor(ctx context.Context, userID int64, items []Item, owner bool) (map[int64][]TagRef, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	rows, err := s.Q.TagsForItems(ctx, db.TagsForItemsParams{UserID: userID, Ids: ids})
	if err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	out := map[int64][]TagRef{}
	for _, r := range rows {
		if !owner && r.Visibility != "public" {
			continue
		}
		out[r.ItemID] = append(out[r.ItemID], TagRef{Slug: r.Slug, Title: r.Title, Visibility: r.Visibility})
	}
	return out, nil
}

// UserArchive is Archive by username, for the API and htmx paging.
func (s *Service) UserArchive(ctx context.Context, viewer *Actor, username, cursor string) (ArchiveView, error) {
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return ArchiveView{}, err
	}
	if u.ShadowBannedAt != nil && (viewer == nil || viewer.ID() != u.ID) && !viewer.IsMod() {
		return ArchiveView{}, ErrNotFound
	}
	return s.Archive(ctx, viewer, u.ID, cursor)
}
