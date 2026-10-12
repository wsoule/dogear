package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/safety"
)

// wordsPerMinute is the reading speed behind an article's reading time.
const wordsPerMinute = 230

// creatorRef is a creator Dogear can identify for a work: the URL of their
// channel or profile, a display name, and the kind ("channel" or "person").
type creatorRef struct {
	URL, Name, Kind string
}

// mediaDetails is what a lookup learns about a link beyond its title.
type mediaDetails struct {
	Creator        *creatorRef
	CreatorName    string // display name even when no creator page is known
	Duration       int    // seconds, videos only
	Published      *time.Time
	ReadingMinutes int // articles only
	SiteName       string
}

// creatorFromURL finds a creator implied by the URL alone: a Substack post
// belongs to its publication and a Medium post to its author's page. The
// name is filled in later from the page.
func creatorFromURL(norm string) *creatorRef {
	u, err := url.Parse(norm)
	if err != nil {
		return nil
	}
	host := safety.Domain(norm)
	if sub, ok := strings.CutSuffix(host, ".substack.com"); ok && sub != "" && sub != "www" && sub != "open" {
		return &creatorRef{URL: "https://" + host, Name: sub, Kind: "channel"}
	}
	if host == "medium.com" {
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(segs) >= 2 && strings.HasPrefix(segs[0], "@") && len(segs[0]) > 1 {
			return &creatorRef{URL: "https://medium.com/" + segs[0], Name: segs[0], Kind: "person"}
		}
	}
	return nil
}

// readingMinutes turns a word count into minutes, rounding up; pages too
// short to be an article get 0.
func readingMinutes(words int) int {
	if words < 150 {
		return 0
	}
	return (words + wordsPerMinute - 1) / wordsPerMinute
}

// parsePublished reads the date formats pages and YouTube use for
// publication times.
func parsePublished(s string) *time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil && t.Year() > 1990 && t.Before(time.Now().Add(48*time.Hour)) {
			return &t
		}
	}
	return nil
}

// lookupMedia gathers details for one link: for a YouTube video its
// channel (oEmbed), length, and publish date (the player endpoint); for
// other pages the site name, author, publish date, and, for articles, the
// reading time.
func (s *Service) lookupMedia(ctx context.Context, norm, kind string) (mediaDetails, error) {
	var d mediaDetails
	if id, ok := safety.YouTubeID(norm); ok {
		var o struct {
			AuthorName string `json:"author_name"`
			AuthorURL  string `json:"author_url"`
		}
		err := s.getJSON(ctx, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape(norm), &o)
		if err == nil && o.AuthorName != "" {
			d.CreatorName = o.AuthorName
			if cn, err := safety.NormalizeURL(o.AuthorURL); err == nil && safety.IsYouTubeChannel(cn) {
				d.Creator = &creatorRef{URL: cn, Name: o.AuthorName, Kind: "channel"}
			}
		}
		d.Duration, d.Published = s.youTubeDetails(ctx, id)
		d.SiteName = "YouTube"
		return d, err
	}
	d.Creator = creatorFromURL(norm)
	page, err := s.fetchPage(ctx, norm, 5*time.Second, 1<<20)
	if err != nil {
		return d, err
	}
	d.SiteName = truncateRunes(safety.SanitizeText(page.SiteName), 80)
	d.CreatorName = page.Author
	d.Published = parsePublished(page.Published)
	if kind == "article" || strings.EqualFold(page.OGType, "article") {
		d.ReadingMinutes = readingMinutes(page.Words)
	}
	if d.Creator != nil {
		switch {
		case d.Creator.Kind == "channel" && page.SiteName != "":
			d.Creator.Name = page.SiteName
		case d.Creator.Kind == "person" && page.Author != "":
			d.Creator.Name = page.Author
		}
		if d.CreatorName == "" {
			d.CreatorName = d.Creator.Name
		}
	}
	return d, nil
}

// youTubeDetails asks YouTube's player endpoint for a video's length and
// publish date. Like transcripts, this is unofficial and best effort: zero
// values mean unknown.
func (s *Service) youTubeDetails(ctx context.Context, videoID string) (int, *time.Time) {
	for _, client := range ytClients {
		body, _ := json.Marshal(map[string]any{"context": map[string]any{"client": client}, "videoId": videoID})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://www.youtube.com/youtubei/v1/player?prettyPrint=false", bytes.NewReader(body))
		if err != nil {
			return 0, nil
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.API.Do(req)
		if err != nil {
			continue
		}
		secs, published := parsePlayerDetails(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if secs > 0 {
			return secs, published
		}
	}
	return 0, nil
}

// parsePlayerDetails reads lengthSeconds and the publish date from a
// player endpoint response.
func parsePlayerDetails(r io.Reader) (int, *time.Time) {
	var p struct {
		VideoDetails struct {
			LengthSeconds string `json:"lengthSeconds"`
		} `json:"videoDetails"`
		Microformat struct {
			Renderer struct {
				PublishDate string `json:"publishDate"`
			} `json:"playerMicroformatRenderer"`
		} `json:"microformat"`
	}
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return 0, nil
	}
	secs, _ := strconv.Atoi(p.VideoDetails.LengthSeconds)
	return secs, parsePublished(p.Microformat.Renderer.PublishDate)
}

// errNoNewCreator means the creator has no page yet and this work may not
// add one.
var errNoNewCreator = errors.New("creator page not created")

// ensureCreator returns the item id for a creator's page. With create, it
// adds the page as an automatic item (via "auto") when none exists. Callers
// pass create only for works that are imports or posted by trusted users,
// since the page goes live without review; its name is checked like a
// post's title, and a blocked domain never gets a page.
func (s *Service) ensureCreator(ctx context.Context, ref creatorRef, create bool) (int64, error) {
	norm, err := safety.NormalizeURL(ref.URL)
	if err != nil {
		return 0, err
	}
	if it, err := s.Q.GetItemByNorm(ctx, norm); err == nil {
		// A pending or removed page is not linked: the link would 404.
		if it.Status != "live" {
			return 0, errNoNewCreator
		}
		return it.ID, nil
	} else if !notFound(err) {
		return 0, fmt.Errorf("find creator: %w", err)
	}
	if !create {
		return 0, errNoNewCreator
	}
	if blocked, err := s.Q.DomainBlocked(ctx, safety.Domain(norm)); err != nil || blocked {
		return 0, errNoNewCreator
	}
	name := truncateRunes(safety.SanitizeText(ref.Name), 120)
	if name == "" || safety.HasMarkupOrURL(name) || safety.LooksLikeInjection(name) {
		// Fall back to the address, which the work's own review covered.
		name = strings.TrimPrefix(norm, "https://")
	}
	it, err := s.Q.InsertItem(ctx, db.InsertItemParams{Url: norm, UrlNorm: norm, Domain: safety.Domain(norm), Title: truncateRunes(name, 120),
		Via: "auto", Status: "live", Kind: ref.Kind, Flags: []string{}})
	if isUniqueViolation(err) {
		existing, err := s.Q.GetItemByNorm(ctx, norm)
		if err != nil {
			return 0, fmt.Errorf("find creator: %w", err)
		}
		return existing.ID, nil
	} else if err != nil {
		return 0, fmt.Errorf("add creator: %w", err)
	}
	return it.ID, nil
}

// refreshMedia looks up one item's media details and stores them, linking
// it to its creator's item (adding that page only when trusted). It records
// every attempt; a failed one is retried a week later, not every minute.
func (s *Service) refreshMedia(ctx context.Context, id int64, norm, kind string, trusted bool) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	d, lookupErr := s.lookupMedia(cctx, norm, kind)
	if d.CreatorName == "" && d.Creator != nil {
		// The page could not be read (Medium often refuses), but the URL
		// still names the creator.
		d.CreatorName = d.Creator.Name
	}
	p := db.SetItemDetailsParams{ID: id, Creator: truncateRunes(safety.SanitizeText(d.CreatorName), 120),
		SiteName: d.SiteName, PublishedAt: d.Published, Ok: lookupErr == nil}
	if d.Duration > 0 {
		p.DurationSeconds = ptr(int32(d.Duration))
	}
	if d.ReadingMinutes > 0 {
		p.ReadingMinutes = ptr(int16(min(d.ReadingMinutes, 32000)))
	}
	if d.Creator != nil {
		if cid, err := s.ensureCreator(ctx, *d.Creator, trusted); err == nil && cid != id {
			p.CreatorID = &cid
		} else if err != nil && !errors.Is(err, errNoNewCreator) {
			s.Log.Warn("ensure creator", "item", id, "err", err)
		}
	}
	if err := s.Q.SetItemDetails(ctx, p); err != nil {
		return fmt.Errorf("set details: %w", err)
	}
	return lookupErr
}

// BackfillMedia fills media details for up to n items that have never been
// looked at: people's posts first, then imports by rank.
func (s *Service) BackfillMedia(ctx context.Context, n int) {
	rows, err := s.Q.ItemsNeedingMedia(ctx, int32(n))
	if err != nil {
		s.Log.Error("media backfill", "err", err)
		return
	}
	found := 0
	for _, r := range rows {
		if err := s.refreshMedia(ctx, r.ID, r.UrlNorm, r.Kind, r.Trusted); err == nil {
			found++
		}
		time.Sleep(time.Second) // stay gentle with YouTube and publishers
	}
	if len(rows) > 0 {
		s.Log.Info("media batch", "tried", len(rows), "found", found)
	}
}

// CreatorWorks returns a creator's works on Dogear as the viewer sees them.
func (s *Service) CreatorWorks(ctx context.Context, viewer *Actor, creatorID int64, n int) ([]Item, error) {
	rows, err := s.Q.CreatorWorks(ctx, db.CreatorWorksParams{CreatorID: &creatorID, Limit: int32(n)})
	if err != nil {
		return nil, fmt.Errorf("creator works: %w", err)
	}
	var keep []db.Item
	for _, r := range rows {
		if ok, err := s.visibleTo(ctx, viewer, r); err == nil && ok {
			keep = append(keep, r)
		}
	}
	return s.decorate(ctx, viewer, keep)
}
