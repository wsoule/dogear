package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"dogear/internal/classify"
	"dogear/internal/db"
	"dogear/internal/importfile"
	"dogear/internal/safety"
)

// importRules limits how often one account can run an import; each one can
// touch thousands of rows.
var importRules = []safety.Rule{{Limit: 10, Per: time.Hour}}

// maxSkipSamples is how many skipped links an import result describes.
const maxSkipSamples = 8

// ImportOptions are the choices on the import form.
type ImportOptions struct {
	// PublicTags creates the file's new tags as public; by default they are
	// private so nothing in an old export is published by surprise.
	PublicTags bool
}

// ImportResult summarizes an import for the person who ran it.
type ImportResult struct {
	Format string `json:"format"`
	// Links is how many links the file held, before duplicates were merged.
	Links int `json:"links"`
	// Saved counts links already on Dogear that are now in the archive;
	// Created counts links new to Dogear, added as personal items and saved.
	Saved        int `json:"saved"`
	Created      int `json:"created"`
	AlreadySaved int `json:"already_saved"`
	Skipped      int `json:"skipped"`
	// SkipSamples describes a few skipped links and why.
	SkipSamples []string `json:"skip_samples,omitempty"`
	TagsCreated int      `json:"tags_created"`
	Tagged      int      `json:"tagged"`
	// Stopped explains a limit that cut the import short.
	Stopped string `json:"stopped,omitempty"`
	// Notes are other things worth knowing, like tags that were left out.
	Notes []string `json:"notes,omitempty"`
}

func (r *ImportResult) skip(link, reason string) {
	r.Skipped++
	if len(r.SkipSamples) < maxSkipSamples {
		r.SkipSamples = append(r.SkipSamples, truncateRunes(link, 80)+" — "+reason)
	}
}

// ParseImportFile reads an uploaded export for ImportSaves, turning parse
// failures into messages the uploader can act on. The web form and the API
// share it so both explain a bad file the same way.
func ParseImportFile(name string, r io.Reader) (importfile.Result, error) {
	res, err := importfile.Parse(name, r)
	switch {
	case errors.Is(err, importfile.ErrTooLarge):
		return res, ErrImportTooLarge
	case errors.Is(err, importfile.ErrUnknownFormat):
		return res, errf("invalid", "Dogear couldn't read that file. Upload a Pocket or Raindrop CSV, a bookmarks HTML file, "+
			"or a YouTube playlist CSV from Google Takeout (or the zip it came in).")
	case err != nil:
		return res, err
	}
	if len(res.Entries) == 0 {
		return res, errf("invalid", "That file has no links in it.")
	}
	return res, nil
}

// ErrImportTooLarge is the message for an upload over the size limit.
var ErrImportTooLarge = errf("invalid", "That file is over %d MB. Split it, or export only part of your saves.", importfile.MaxBytes>>20)

// importLink is one distinct link from a file after normalizing: entries
// with the same address are merged, keeping the earliest save time, the
// first title and note, and every tag.
type importLink struct {
	norm, domain, title, note string
	untitled                  bool
	savedAt                   time.Time
	tags                      []string // tag slugs
	itemID                    int64
	saved                     bool // already in the archive before this import
}

// tagSlugOK is false for tag names with nothing a slug can keep (emoji or
// non-Latin names), which would otherwise all collapse into one tag.
var tagSlugOK = regexp.MustCompile(`[A-Za-z0-9]`)

// importTitle cleans a title from an export, falling back to one made from
// the address when it is missing or unusable (empty, a bare URL, markup).
// It reports whether the fallback was used.
func importTitle(raw, norm, domain string) (string, bool) {
	t := truncateRunes(safety.SanitizeText(raw), 120)
	if t != "" && !safety.HasMarkupOrURL(t) && !safety.LooksLikeInjection(t) {
		return t, false
	}
	return fallbackTitle(norm, domain), true
}

// fallbackTitle names a link by its address, "example.com/blog/post", or
// "YouTube video <id>", until the media backfill finds its real title.
func fallbackTitle(norm, domain string) string {
	if id, ok := safety.YouTubeID(norm); ok {
		return "YouTube video " + id
	}
	t := domain
	if u, err := url.Parse(norm); err == nil {
		p := strings.Trim(u.Path, "/")
		if dp, err := url.PathUnescape(p); err == nil {
			p = dp
		}
		if p = safety.SanitizeText(p); p != "" {
			t += "/" + p
		}
	}
	return truncateRunes(t, 120)
}

// prepareImport normalizes and merges a file's entries, dropping links that
// are invalid, shortened, or on blocked domains. It returns the links in
// file order and each tag slug's display title.
func (s *Service) prepareImport(ctx context.Context, entries []importfile.Entry, res *ImportResult) ([]*importLink, map[string]string, error) {
	var links []*importLink
	byNorm := map[string]*importLink{}
	tagTitles := map[string]string{}
	for _, e := range entries {
		raw := strings.TrimSpace(e.URL)
		if len(raw) > 2048 {
			res.skip(raw, "address too long")
			continue
		}
		norm, err := safety.NormalizeURL(raw)
		if err != nil {
			res.skip(raw, "not a public web link")
			continue
		}
		domain := safety.Domain(norm)
		if safety.IsShortener(domain) {
			res.skip(raw, "link shortener")
			continue
		}
		var slugs []string
		for _, t := range e.Tags {
			title := truncateRunes(safety.SanitizeText(t), 80)
			if !tagSlugOK.MatchString(title) || safety.HasMarkupOrURL(title) {
				continue
			}
			slug := Slugify(title)
			if _, ok := tagTitles[slug]; !ok {
				tagTitles[slug] = title
			}
			if !slices.Contains(slugs, slug) {
				slugs = append(slugs, slug)
			}
		}
		note := truncateRunes(safety.SanitizeText(e.Note), 280)
		if safety.HasMarkupOrURL(note) {
			note = ""
		}
		if l := byNorm[norm]; l != nil {
			for _, t := range slugs {
				if !slices.Contains(l.tags, t) {
					l.tags = append(l.tags, t)
				}
			}
			if l.untitled && e.Title != "" {
				l.title, l.untitled = importTitle(e.Title, norm, domain)
			}
			if l.note == "" {
				l.note = note
			}
			if !e.SavedAt.IsZero() && (l.savedAt.IsZero() || e.SavedAt.Before(l.savedAt)) {
				l.savedAt = e.SavedAt
			}
			continue
		}
		l := &importLink{norm: norm, domain: domain, note: note, savedAt: e.SavedAt, tags: slugs}
		l.title, l.untitled = importTitle(e.Title, norm, domain)
		byNorm[norm] = l
		links = append(links, l)
	}
	if len(links) == 0 {
		return nil, tagTitles, nil
	}
	domains := make([]string, 0, len(links))
	for _, l := range links {
		if !slices.Contains(domains, l.domain) {
			domains = append(domains, l.domain)
		}
	}
	blocked, err := s.Q.BlockedAmong(ctx, domains)
	if err != nil {
		return nil, nil, fmt.Errorf("blocked domains: %w", err)
	}
	if len(blocked) > 0 {
		links = slices.DeleteFunc(links, func(l *importLink) bool {
			if slices.Contains(blocked, l.domain) {
				res.skip(l.norm, "links to "+l.domain+" are not accepted")
				return true
			}
			return false
		})
	}
	return links, tagTitles, nil
}

// ImportSaves brings an export's links into the actor's archive. Links
// already on Dogear are saved as they are; new ones become the actor's
// personal items (not in feeds or search, see migration 00016) without the
// posting checks, classifier, or post limits, since nothing reaches a
// public feed until someone posts it. Tags from the file become the actor's
// tags, private unless opts.PublicTags. It keeps save times from the file,
// never overwrites an existing save's note, and running the same file again
// adds nothing. The archive and tag limits stop or trim the import, and the
// result says so.
func (s *Service) ImportSaves(ctx context.Context, a *Actor, file importfile.Result, opts ImportOptions) (ImportResult, error) {
	res := ImportResult{Format: file.Format.Label(), Links: len(file.Entries)}
	if err := require(a, "post"); err != nil {
		return res, err
	}
	if err := s.limit("import:"+strconv.FormatInt(a.ID(), 10), importRules...); err != nil {
		return res, err
	}
	if file.Truncated {
		res.Notes = append(res.Notes, fmt.Sprintf("Only the first %d links in the file were read.", importfile.MaxEntries))
	}
	links, tagTitles, err := s.prepareImport(ctx, file.Entries, &res)
	if err != nil {
		return res, err
	}
	if len(links) == 0 {
		return res, nil
	}
	via := a.Via
	if via == "" {
		via = "web"
	}
	now := time.Now()
	err = s.inTxQ(ctx, func(q *db.Queries) error {
		if err := q.LockUserForImport(ctx, a.ID()); err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		kept, created, err := importItems(ctx, q, a, via, links, &res)
		if err != nil {
			return err
		}
		if err := importSavesOf(ctx, q, a, kept, created, now, &res); err != nil {
			return err
		}
		return s.importTags(ctx, q, a, kept, tagTitles, opts, now, &res)
	})
	if err != nil {
		return ImportResult{Format: res.Format, Links: res.Links}, err
	}
	return res, nil
}

// importItems finds each link's item, trims the list to what the archive
// can still hold, and adds the remaining new links as personal items. It
// returns the links that go in the archive, each with its itemID, and the
// ids of the items it created.
func importItems(ctx context.Context, q *db.Queries, a *Actor, via string, links []*importLink, res *ImportResult) ([]*importLink, map[int64]bool, error) {
	norms := make([]string, len(links))
	for i, l := range links {
		norms[i] = l.norm
	}
	existing, err := q.ItemsByNorms(ctx, db.ItemsByNormsParams{UserID: a.ID(), Norms: norms})
	if err != nil {
		return nil, nil, fmt.Errorf("find items: %w", err)
	}
	type found struct {
		id      int64
		status  string
		visible bool
	}
	byNorm := make(map[string]found, len(existing))
	ids := make([]int64, 0, len(existing))
	for _, it := range existing {
		byNorm[it.UrlNorm] = found{it.ID, it.Status, it.Visible}
		ids = append(ids, it.ID)
	}
	saved := map[int64]bool{}
	if len(ids) > 0 {
		rows, err := q.SavedAmong(ctx, db.SavedAmongParams{UserID: a.ID(), Ids: ids})
		if err != nil {
			return nil, nil, fmt.Errorf("saved: %w", err)
		}
		for _, r := range rows {
			saved[r.ItemID] = true
		}
	}
	count, err := q.CountSaves(ctx, a.ID())
	if err != nil {
		return nil, nil, fmt.Errorf("count saves: %w", err)
	}
	room := maxSavesPerUser - int(count)
	var keep []*importLink
	for _, l := range links {
		f, ok := byNorm[l.norm]
		switch {
		case ok && f.status == "removed":
			res.skip(l.norm, "removed from Dogear")
			continue
		case ok && !f.visible:
			// Waiting for review, or posted by someone hidden from you:
			// a normal save would 404 too.
			res.skip(l.norm, "not available on Dogear")
			continue
		case ok && saved[f.id]:
			l.itemID, l.saved = f.id, true
			res.AlreadySaved++
		case room <= 0:
			if res.Stopped == "" {
				res.Stopped = fmt.Sprintf("Your archive is full: it holds at most %d things, so the rest of the file was not imported.", maxSavesPerUser)
			}
			res.Skipped++
			continue
		default:
			room--
			l.itemID = f.id // 0 when the link is new to Dogear
		}
		keep = append(keep, l)
	}
	created := map[int64]bool{}
	var p db.BulkInsertPersonalItemsParams
	for _, l := range keep {
		if l.itemID == 0 {
			p.Urls = append(p.Urls, l.norm)
			p.Domains = append(p.Domains, l.domain)
			p.Titles = append(p.Titles, l.title)
			p.Kinds = append(p.Kinds, classify.KindFor("", l.norm, classify.Result{}))
			p.Untitled = append(p.Untitled, l.untitled)
		}
	}
	if len(p.Urls) > 0 {
		p.CreatedBy, p.TokenID, p.Via = a.ID(), a.TokenID, via
		inserted, err := q.BulkInsertPersonalItems(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("add items: %w", err)
		}
		newIDs := make(map[string]int64, len(inserted))
		for _, r := range inserted {
			newIDs[r.UrlNorm] = r.ID
			created[r.ID] = true
		}
		// A link someone else added after the lookup above is saved as
		// theirs.
		var raced []string
		for _, l := range keep {
			if l.itemID == 0 {
				if id, ok := newIDs[l.norm]; ok {
					l.itemID = id
				} else {
					raced = append(raced, l.norm)
				}
			}
		}
		if len(raced) > 0 {
			rows, err := q.ItemsByNorms(ctx, db.ItemsByNormsParams{UserID: a.ID(), Norms: raced})
			if err != nil {
				return nil, nil, fmt.Errorf("find items: %w", err)
			}
			for _, r := range rows {
				if !r.Visible {
					continue // dropped below, like any link it could not save
				}
				for _, l := range keep {
					if l.norm == r.UrlNorm && l.itemID == 0 {
						l.itemID = r.ID
					}
				}
			}
		}
	}
	keep = slices.DeleteFunc(keep, func(l *importLink) bool { return l.itemID == 0 })
	return keep, created, nil
}

// importSavesOf saves every link not already in the archive, counting items
// this import created separately from links that were already on Dogear.
func importSavesOf(ctx context.Context, q *db.Queries, a *Actor, links []*importLink, created map[int64]bool, now time.Time, res *ImportResult) error {
	var p db.BulkInsertSavesParams
	for _, l := range links {
		if l.saved {
			continue
		}
		at := l.savedAt
		if at.IsZero() {
			at = now
		}
		p.ItemIds = append(p.ItemIds, l.itemID)
		p.Notes = append(p.Notes, l.note)
		p.SavedAt = append(p.SavedAt, at)
	}
	if len(p.ItemIds) == 0 {
		return nil
	}
	p.UserID = a.ID()
	ids, err := q.BulkInsertSaves(ctx, p)
	if err != nil {
		return fmt.Errorf("save items: %w", err)
	}
	for _, id := range ids {
		if created[id] {
			res.Created++
		} else {
			res.Saved++
		}
	}
	_ = q.MarkDirty(ctx, a.ID())
	return nil
}

// importTags creates the file's tags the actor does not have yet (up to the
// tag limit) and tags each link, skipping entries a tag already holds and
// any past a tag's size limit.
func (s *Service) importTags(ctx context.Context, q *db.Queries, a *Actor, links []*importLink, titles map[string]string,
	opts ImportOptions, now time.Time, res *ImportResult) error {
	var wanted []string
	var itemIDs []int64
	for _, l := range links {
		itemIDs = append(itemIDs, l.itemID)
		for _, t := range l.tags {
			if !slices.Contains(wanted, t) {
				wanted = append(wanted, t)
			}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	mine, err := q.UserLists(ctx, a.ID())
	if err != nil {
		return fmt.Errorf("tags: %w", err)
	}
	type tag struct {
		id         int64
		count      int
		visibility string
	}
	tags := map[string]*tag{}
	for _, l := range mine {
		tags[l.Slug] = &tag{id: l.ID, count: int(l.EntryCount), visibility: l.Visibility}
	}
	vis := "private"
	if opts.PublicTags {
		vis = "public"
	}
	var dropped []string
	for _, slug := range wanted {
		if tags[slug] != nil {
			continue
		}
		if len(tags) >= maxListsPerUser {
			dropped = append(dropped, slug)
			continue
		}
		l, err := q.CreateList(ctx, db.CreateListParams{UserID: a.ID(), Slug: slug, Title: titles[slug], Visibility: vis})
		if err != nil {
			return fmt.Errorf("create tag: %w", err)
		}
		tags[slug] = &tag{id: l.ID, visibility: l.Visibility}
		res.TagsCreated++
	}
	if len(dropped) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("You can have at most %d tags, so %d tags from the file were left out (%s).",
			maxListsPerUser, len(dropped), strings.Join(dropped[:min(len(dropped), 5)], ", ")))
	}
	listIDs := make([]int64, 0, len(tags))
	for _, t := range tags {
		listIDs = append(listIDs, t.id)
	}
	have, err := q.TagEntriesAmong(ctx, db.TagEntriesAmongParams{ListIds: listIDs, ItemIds: itemIDs})
	if err != nil {
		return fmt.Errorf("tag entries: %w", err)
	}
	inTag := make(map[[2]int64]bool, len(have))
	for _, h := range have {
		inTag[[2]int64{h.ListID, h.ItemID}] = true
	}
	var p db.BulkInsertTagEntriesParams
	touched := map[int64]bool{}
	full := map[string]bool{}
	public := false
	for _, l := range links {
		at := l.savedAt
		if at.IsZero() {
			at = now
		}
		for _, slug := range l.tags {
			t := tags[slug]
			if t == nil || inTag[[2]int64{t.id, l.itemID}] {
				continue
			}
			if t.count >= maxEntriesPerList {
				full[slug] = true
				continue
			}
			t.count++
			inTag[[2]int64{t.id, l.itemID}] = true
			p.ListIds = append(p.ListIds, t.id)
			p.ItemIds = append(p.ItemIds, l.itemID)
			p.AddedAt = append(p.AddedAt, at)
			p.Ords = append(p.Ords, int32(len(p.Ords)))
			touched[t.id] = true
			public = public || t.visibility == "public"
		}
	}
	if len(full) > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("A tag can hold at most %d things; %d tags filled up.", maxEntriesPerList, len(full)))
	}
	if len(p.ListIds) == 0 {
		return nil
	}
	n, err := q.BulkInsertTagEntries(ctx, p)
	if err != nil {
		return fmt.Errorf("tag items: %w", err)
	}
	res.Tagged = int(n)
	ids := make([]int64, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	if err := q.TouchLists(ctx, ids); err != nil {
		return fmt.Errorf("touch tags: %w", err)
	}
	if err := q.MarkListLinksDirty(ctx, ids); err != nil {
		return fmt.Errorf("mark links: %w", err)
	}
	if public {
		_ = q.MarkDirty(ctx, a.ID())
	}
	return nil
}
