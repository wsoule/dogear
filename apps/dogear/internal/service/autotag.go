package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/importfile"
	"dogear/internal/safety"
)

const (
	// minAutoTagProb is how sure jev must be before a save is filed under
	// its pick; below it the keyword fallback decides, or nothing does.
	minAutoTagProb = 0.5
	// autoTagJevTimeout caps the jev call so a slow answer falls back to
	// keywords instead of holding up a save.
	autoTagJevTimeout = 5 * time.Second
	autoTagNone       = "none"
)

// autoTagState is what jev sees when picking a tag: the saved link and the
// saver's tags. Field names are referenced from the question with backticks.
type autoTagState struct {
	Item struct {
		Title   string `json:"title"`
		Reason  string `json:"reason"`
		URL     string `json:"url"`
		Kind    string `json:"kind"`
		Creator string `json:"creator"`
	} `json:"item"`
	Tags []string `json:"tags"`
}

// AutoTag files a save that has no tags yet under one of the saver's tags,
// so grouping takes no work. With jev configured (Grouper), jev picks the
// best of the saver's tags, or a topic (AI, Design, ...) to start a new tag
// from. Without jev, or when jev is unsure, slow, or down, the first topic
// whose keywords match the item decides: the saver's tag of that name, or a
// new public one. It returns the tag slug used, or "" when it filed nothing.
// A failure here never undoes the save.
func (s *Service) AutoTag(ctx context.Context, a *Actor, itemID int64) (string, error) {
	tagged, err := s.Q.TagsForItems(ctx, db.TagsForItemsParams{UserID: a.ID(), Ids: []int64{itemID}})
	if err != nil {
		return "", fmt.Errorf("tags: %w", err)
	}
	if len(tagged) > 0 {
		return "", nil // the saver already grouped it
	}
	it, err := s.Q.GetItem(ctx, itemID)
	if err != nil {
		return "", fmt.Errorf("item: %w", err)
	}
	lists, err := s.userLists(ctx, a.ID(), true)
	if err != nil {
		return "", err
	}
	lists = slices.DeleteFunc(lists, func(l List) bool { return l.Slug == legacyDogearSlug })
	topics, err := s.Q.AllTopics(ctx)
	if err != nil {
		return "", fmt.Errorf("topics: %w", err)
	}

	slug, title := "", ""
	if s.Grouper != nil {
		slug, title = s.jevTag(ctx, it, lists, topics)
	}
	if slug == "" && title == "" {
		matched, err := s.Q.TopicsMatching(ctx, it.Title+" "+it.Blurb+" "+it.Creator)
		if err != nil {
			return "", fmt.Errorf("match topics: %w", err)
		}
		if len(matched) == 0 {
			return "", nil
		}
		slug, title = tagForTopic(lists, matched[0].Slug, matched[0].Name)
	}
	if slug == "" {
		l, err := s.createAutoTag(ctx, a, title)
		if err != nil {
			return "", err
		}
		slug = l.Slug
	}
	if err := s.SaveToList(ctx, a, slug, itemID, "", nil); err != nil {
		return "", err
	}
	return slug, nil
}

// jevTag asks jev which of the saver's tags fits the item, or which topic
// should start a new tag. It returns an existing tag's slug, or "" and the
// new tag's title, or two empty strings when jev is unsure or fails.
func (s *Service) jevTag(ctx context.Context, it db.Item, lists []List, topics []db.AllTopicsRow) (slug, title string) {
	options := map[string]string{autoTagNone: "None of these fits the item well."}
	var st autoTagState
	st.Item.Title, st.Item.Reason, st.Item.URL, st.Item.Kind, st.Item.Creator = it.Title, it.Blurb, it.Url, it.Kind, it.Creator
	for _, l := range lists {
		desc := "The saver's tag \"" + l.Title + "\""
		if l.Description != "" {
			desc += ": " + l.Description
		}
		options["tag:"+l.Slug] = desc
		st.Tags = append(st.Tags, l.Title)
	}
	for _, t := range topics {
		if slices.ContainsFunc(lists, func(l List) bool { return l.Slug == t.Slug }) {
			continue
		}
		options["new:"+t.Slug] = "None of the saver's tags fit; start a new tag \"" + t.Name + "\" for " + t.Description
	}
	jctx, cancel := context.WithTimeout(ctx, autoTagJevTimeout)
	defer cancel()
	choice, prob, err := s.Grouper.Choose(jctx, st,
		"Which of the saver's `tags` should `item` be filed under? Prefer an existing tag when one fits; pick a new tag only when none does.", options)
	if err != nil {
		s.Log.Warn("auto tag: jev", "item", it.ID, "err", err)
		return "", ""
	}
	if prob < minAutoTagProb || choice == autoTagNone {
		return "", ""
	}
	if s, ok := strings.CutPrefix(choice, "tag:"); ok {
		return s, ""
	}
	if ts, ok := strings.CutPrefix(choice, "new:"); ok {
		for _, t := range topics {
			if t.Slug == ts {
				return tagForTopic(lists, t.Slug, t.Name)
			}
		}
	}
	return "", ""
}

// tagForTopic returns the saver's tag for a topic when they have one (same
// slug or title), else "" and the topic's name for a new tag.
func tagForTopic(lists []List, topicSlug, topicName string) (slug, title string) {
	for _, l := range lists {
		if l.Slug == topicSlug || strings.EqualFold(l.Title, topicName) {
			return l.Slug, ""
		}
	}
	return "", topicName
}

// createAutoTag makes a public tag for AutoTag. Unlike CreateList it skips
// the tag-creation rate limit (the user isn't typing these), but keeps the
// per-user cap on tags.
func (s *Service) createAutoTag(ctx context.Context, a *Actor, title string) (List, error) {
	n, err := s.Q.CountUserLists(ctx, a.ID())
	if err != nil {
		return List{}, fmt.Errorf("count lists: %w", err)
	}
	if n >= maxListsPerUser {
		return List{}, errf("invalid", "you can have at most %d tags", maxListsPerUser)
	}
	slug, err := uniqueListSlug(ctx, s.Q, a.ID(), title, 0)
	if err != nil {
		return List{}, err
	}
	l, err := s.Q.CreateList(ctx, db.CreateListParams{UserID: a.ID(), Slug: slug, Title: title, Visibility: "public"})
	if err != nil {
		return List{}, fmt.Errorf("create tag: %w", err)
	}
	s.markDirty(ctx, a.ID())
	return toList(l, 0), nil
}

// autoTagLater runs AutoTag after a one-tap dogear without holding up the
// tap. Tests set AutoTagSync to run it in line.
func (s *Service) autoTagLater(a *Actor, itemID int64) {
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := s.AutoTag(ctx, a, itemID); err != nil {
			s.Log.Warn("auto tag", "item", itemID, "err", err)
		}
	}
	if s.AutoTagSync {
		run()
		return
	}
	go run()
}

// QuickSave saves a link to the actor's archive in one step, from a pasted
// URL or the phone's share sheet: no title, reason, or category to fill in.
// A link already on Dogear is saved as it is; a new one becomes a personal
// item (like an import) titled from its page when that loads quickly, and
// can be posted publicly with a reason later. The save is then filed under a
// tag (AutoTag). It returns the item's id and the tag slug, if any.
func (s *Service) QuickSave(ctx context.Context, a *Actor, rawURL string) (int64, string, error) {
	if err := require(a, "post"); err != nil {
		return 0, "", err
	}
	if err := s.limit(fmt.Sprintf("quicksave:%d", a.ID()), quickSaveRules...); err != nil {
		return 0, "", err
	}
	entry := importfile.Entry{URL: strings.TrimSpace(rawURL)}
	if norm, err := safety.NormalizeURL(entry.URL); err == nil {
		ectx, cancel := context.WithTimeout(ctx, 4*time.Second)
		if e, err := s.Enrich(ectx, norm); err == nil {
			entry.Title = e.Title
		}
		cancel()
	}
	res, kept, err := s.importEntries(ctx, a, []importfile.Entry{entry}, ImportOptions{PublicTags: true})
	if err != nil {
		return 0, "", err
	}
	if len(kept) == 0 {
		msg := "that link can't be saved"
		if len(res.SkipSamples) > 0 {
			if _, why, ok := strings.Cut(res.SkipSamples[0], " — "); ok {
				msg = "that link can't be saved: " + why
			}
		}
		if res.Stopped != "" {
			msg = res.Stopped
		}
		return 0, "", errf("invalid", "%s", msg)
	}
	id := kept[0].itemID
	slug, err := s.AutoTag(ctx, a, id)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("auto tag", "item", id, "err", err)
	}
	return id, slug, nil
}

// TagGroup is one tag on a profile: the tag and its first few entries.
type TagGroup struct {
	List
	Entries []Entry
	More    int // entries beyond the ones shown
}

const (
	maxProfileGroups  = 12
	profileGroupItems = 4
)

// TagGroups returns a profile's tags as groups the viewer may see, each with
// its first few entries, largest tags first, so an archive reads as
// "here's what I keep, by subject". Empty and hidden tags are left out.
func (s *Service) TagGroups(ctx context.Context, viewer *Actor, username string, lists []List) []TagGroup {
	ordered := slices.Clone(lists)
	slices.SortStableFunc(ordered, func(a, b List) int { return b.Count - a.Count })
	var out []TagGroup
	for _, l := range ordered {
		if len(out) == maxProfileGroups {
			break
		}
		if l.Count == 0 || l.Slug == legacyDogearSlug {
			continue
		}
		lv, err := s.GetList(ctx, viewer, username, l.Slug)
		if err != nil || len(lv.Entries) == 0 {
			continue // hidden from this viewer, or emptied since
		}
		g := TagGroup{List: lv.List, Entries: lv.Entries}
		g.Count = len(lv.Entries)
		if len(g.Entries) > profileGroupItems {
			g.More = len(g.Entries) - profileGroupItems
			g.Entries = g.Entries[:profileGroupItems]
		}
		out = append(out, g)
	}
	return out
}

// CheckGrouper asks jev one small question at startup and logs whether it
// answered, so a bad key or a changed API shows up in the logs right after
// a deploy instead of as silently untagged saves.
func (s *Service) CheckGrouper(ctx context.Context) {
	if s.Grouper == nil {
		s.Log.Info("auto tag: jev off (no JEV_API_KEY); grouping by topic keywords")
		return
	}
	var st autoTagState
	st.Item.Title, st.Item.Reason, st.Item.URL, st.Item.Kind = "Attention Is All You Need",
		"The paper that introduced the transformer.", "https://arxiv.org/abs/1706.03762", "article"
	st.Tags = []string{"AI", "Cooking"}
	choice, prob, err := s.Grouper.Choose(ctx, st, "Which of the saver's `tags` should `item` be filed under?",
		map[string]string{"tag:ai": "The saver's tag \"AI\"", "tag:cooking": "The saver's tag \"Cooking\"", autoTagNone: "None of these fits."})
	if err != nil {
		s.Log.Error("auto tag: jev check failed", "err", err)
		return
	}
	s.Log.Info("auto tag: jev ok", "choice", choice, "prob", prob)
}

// GroupResult is what GroupUntagged did.
type GroupResult struct {
	Looked, Tagged int
}

// maxGroupBatch caps one "group my untagged saves" run.
const maxGroupBatch = 500

// GroupUntagged runs AutoTag over the actor's saves that have no tag yet,
// newest first, for archives saved before auto-tagging existed. Each save
// is filed independently; one failure doesn't stop the rest.
func (s *Service) GroupUntagged(ctx context.Context, a *Actor) (GroupResult, error) {
	var res GroupResult
	if err := require(a, "post"); err != nil {
		return res, err
	}
	ids, err := s.Q.UntaggedSaves(ctx, db.UntaggedSavesParams{UserID: a.ID(), Lim: maxGroupBatch})
	if err != nil {
		return res, fmt.Errorf("untagged saves: %w", err)
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		res.Looked++
		slug, err := s.AutoTag(ctx, a, id)
		if err != nil {
			s.Log.Warn("group untagged", "item", id, "err", err)
			continue
		}
		if slug != "" {
			res.Tagged++
		}
	}
	return res, nil
}

// GroupUntaggedLater starts GroupUntagged in the background (it can take a
// few minutes with jev), at most a few times an hour per user.
func (s *Service) GroupUntaggedLater(a *Actor) error {
	if err := require(a, "post"); err != nil {
		return err
	}
	if err := s.limit(fmt.Sprintf("group:%d", a.ID()), groupRules...); err != nil {
		return err
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := s.GroupUntagged(ctx, a)
		if err != nil {
			s.Log.Warn("group untagged", "user", a.ID(), "err", err)
			return
		}
		s.Log.Info("grouped untagged saves", "user", a.ID(), "looked", res.Looked, "tagged", res.Tagged)
	}
	if s.AutoTagSync {
		run()
		return nil
	}
	go run()
	return nil
}
