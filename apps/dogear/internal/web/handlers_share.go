package web

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/service"
)

// Share surfaces: preview images, RSS feeds, the embeddable tag page, and the
// PWA share target. Everything here except /share renders the signed-out
// view, whoever asks, because the output is meant to be passed around.

// feedSize is how many entries an RSS feed or embed carries.
const feedSize = 50

// ogItems is how many item titles a preview image lists.
const ogItems = 4

// ownerName is a user's display name, or their username when unset.
func ownerName(u db.User) string {
	if u.DisplayName != nil && strings.TrimSpace(*u.DisplayName) != "" {
		return strings.TrimSpace(*u.DisplayName)
	}
	return u.Username
}

func things(n int) string {
	if n == 1 {
		return "1 thing"
	}
	return commas(n) + " things"
}

// siteHost is BaseURL without its scheme, for footers like "dogear.fyi/u/x".
func (s *Server) siteHost() string {
	h := strings.TrimPrefix(strings.TrimPrefix(s.cfg.BaseURL, "https://"), "http://")
	if h == "" {
		return "dogear.fyi"
	}
	return h
}

// archivePath and tagPath are the public page paths share links point at.
func archivePath(username string) string { return "/u/" + url.PathEscape(username) }
func tagPath(username, slug string) string {
	return archivePath(username) + "/" + url.PathEscape(slug)
}

// Preview image URLs live under /og/ rather than /u/: robots.txt disallows
// /u/, and some card crawlers honor that for images too.
func ogArchivePath(username string) string { return "/og/u/" + url.PathEscape(username) + ".png" }
func ogTagPath(username, slug string) string {
	return "/og/u/" + url.PathEscape(username) + "/" + url.PathEscape(slug) + ".png"
}

func entryTitles(es []service.Entry, n int) []string {
	var out []string
	for i := 0; i < len(es) && i < n; i++ {
		out = append(out, es[i].Item.Title)
	}
	return out
}

// ogArchive serves /og/u/{username}.png, the preview card for an archive.
func (s *Server) ogArchive(w http.ResponseWriter, r *http.Request) {
	username, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok {
		s.notFound(w, r)
		return
	}
	a, err := s.svc.PublicArchive(r.Context(), username)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	items := entryTitles(a.Entries, ogItems)
	s.writeOG(w, r, ogCard{Title: ownerName(a.User) + "'s dogear", Byline: "by @" + a.User.Username + " · " + things(a.Count),
		Items: items, More: a.Count - len(items), Footer: s.siteHost() + archivePath(a.User.Username)})
}

// ogTag serves /og/u/{username}/{slug}.png, the preview card for a public
// tag. Unlisted and private tags are not found.
func (s *Server) ogTag(w http.ResponseWriter, r *http.Request) {
	slug, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok {
		s.notFound(w, r)
		return
	}
	t, err := s.svc.PublicTag(r.Context(), r.PathValue("username"), slug)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	items := entryTitles(t.Entries, ogItems)
	s.writeOG(w, r, ogCard{Title: t.Title, Byline: "by @" + t.Owner + " · " + things(len(t.Entries)),
		Items: items, More: len(t.Entries) - len(items), Footer: s.siteHost() + tagPath(t.Owner, t.Slug)})
}

// writeOG serves a preview card from the PNG cache, rendering it on a miss.
// The ETag is the card's content hash, so unchanged cards revalidate cheaply.
func (s *Server) writeOG(w http.ResponseWriter, r *http.Request, c ogCard) {
	key := c.key()
	etag := `"og-` + key + `"`
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, ok := s.ogCache.get(key)
	if !ok {
		var err error
		if b, err = renderOG(c); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.ogCache.put(key, b)
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(b)
}

// RSS 2.0 documents. atom:link rel=self is what feed validators expect.
type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Atom    string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Self        rssLink   `xml:"atom:link"`
	LastBuild   string    `xml:"lastBuildDate,omitempty"`
	Items       []rssItem `xml:"item"`
}

type rssLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	Description string  `xml:"description,omitempty"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

// rssItems turns entries into feed items: the link is the original URL, the
// description is the owner's note (or the item's blurb), and the guid is
// the item's Dogear page so readers dedupe across feeds.
func (s *Server) rssItems(es []service.Entry) []rssItem {
	out := make([]rssItem, 0, len(es))
	for _, e := range es {
		desc := e.Note
		if desc == "" {
			desc = e.Item.Blurb
		}
		out = append(out, rssItem{Title: e.Item.Title, Link: e.Item.URL, Description: desc,
			GUID:    rssGUID{Value: s.cfg.BaseURL + "/i/" + strconv.FormatInt(e.Item.ID, 10), IsPermaLink: true},
			PubDate: e.AddedAt.UTC().Format(time.RFC1123Z)})
	}
	return out
}

func (s *Server) writeRSS(w http.ResponseWriter, ch rssChannel) {
	if len(ch.Items) > 0 {
		ch.LastBuild = ch.Items[0].PubDate
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=900")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(rssDoc{Version: "2.0", Atom: "http://www.w3.org/2005/Atom", Channel: ch})
}

// rssArchive serves /u/{username}/feed.xml: the newest public saves.
// Slugs never contain a dot, so this can't shadow a tag page.
func (s *Server) rssArchive(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.PublicArchive(r.Context(), r.PathValue("username"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	es := a.Entries
	if len(es) > feedSize {
		es = es[:feedSize]
	}
	page := s.cfg.BaseURL + archivePath(a.User.Username)
	s.writeRSS(w, rssChannel{Title: ownerName(a.User) + "'s dogear", Link: page,
		Description: "Things @" + a.User.Username + " saved on Dogear, newest first.",
		Self:        rssLink{Href: page + "/feed.xml", Rel: "self", Type: "application/rss+xml"}, Items: s.rssItems(es)})
}

// rssTag serves /u/{username}/{slug}/feed.xml for public tags, newest
// tagged first.
func (s *Server) rssTag(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.PublicTag(r.Context(), r.PathValue("username"), r.PathValue("slug"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	es := t.Entries
	service.SortEntries(es, "added")
	if len(es) > feedSize {
		es = es[:feedSize]
	}
	page := s.cfg.BaseURL + tagPath(t.Owner, t.Slug)
	desc := t.Description
	if desc == "" {
		desc = "Things @" + t.Owner + " tagged #" + t.Slug + " on Dogear."
	}
	s.writeRSS(w, rssChannel{Title: t.Title + " · @" + t.Owner, Link: page, Description: desc,
		Self: rssLink{Href: page + "/feed.xml", Rel: "self", Type: "application/rss+xml"}, Items: s.rssItems(es)})
}

// embedPage is the data for the standalone iframe page.
type embedPage struct {
	T       service.SharedTag
	Entries []service.Entry
	More    int
	PageURL string
	Theme   string // "", "light", or "dark"; empty follows the system
}

// embed serves /u/{username}/{slug}/embed: a compact public tag with no
// site chrome, the one page other sites may frame.
func (s *Server) embed(w http.ResponseWriter, r *http.Request) {
	t, err := s.svc.PublicTag(r.Context(), r.PathValue("username"), r.PathValue("slug"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	ep := embedPage{T: t, Entries: t.Entries, PageURL: s.cfg.BaseURL + tagPath(t.Owner, t.Slug)}
	if len(ep.Entries) > feedSize {
		ep.More = len(ep.Entries) - feedSize
		ep.Entries = ep.Entries[:feedSize]
	}
	if th := r.URL.Query().Get("theme"); th == "light" || th == "dark" {
		ep.Theme = th
	}
	allowFraming(w)
	w.Header().Set("Cache-Control", "public, max-age=300")
	s.exec(w, http.StatusOK, "embed", "embed", ep)
}

// allowFraming replaces the site-wide no-framing headers for the embed
// page only. The page runs no scripts and has no forms, so a strict policy
// with any frame ancestor is safe.
func allowFraming(w http.ResponseWriter) {
	h := w.Header()
	h.Del("X-Frame-Options")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; font-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors *")
}

// embedSnippet is the copyable iframe code shown on a public tag page.
func (s *Server) embedSnippet(lv service.ListView) string {
	src := s.cfg.BaseURL + tagPath(lv.Owner, lv.Slug) + "/embed"
	title := lv.Title + " · @" + lv.Owner + " on dogear"
	return fmt.Sprintf(`<iframe src="%s" title="%s" width="100%%" height="480" style="border:0" loading="lazy"></iframe>`,
		src, strings.ReplaceAll(title, `"`, "&quot;"))
}

// shareLink matches the first http(s) URL in shared text, e.g. "great read
// https://example.com/a via @x".
var shareLink = regexp.MustCompile(`https?://[^\s<>"]+`)

// shareURL picks the link out of a share-sheet request. Apps disagree on
// where it goes: some send url, many send only text with the link inside.
// It returns "" when there is no http(s) link anywhere.
func shareURL(rawURL, text, title string) string {
	for _, s := range []string{rawURL, text, title} {
		m := shareLink.FindString(s)
		if m == "" {
			continue
		}
		m = strings.TrimRight(m, ".,;:!?)]}'’”")
		if u, err := url.Parse(m); err == nil && u.Host != "" {
			return m
		}
	}
	return ""
}

// share is the PWA share target: a signed-in user lands on the item page
// when the link is already on Dogear (to dogear or tag it), or on the post
// form prefilled with it. Signed-out users sign in first and come back.
func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	if viewer(r) == nil {
		http.Redirect(w, r, "/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	link := shareURL(q.Get("url"), q.Get("text"), q.Get("title"))
	if link == "" {
		http.Redirect(w, r, "/submit", http.StatusSeeOther)
		return
	}
	if id, ok := s.svc.ItemForURL(r.Context(), viewer(r), link); ok {
		http.Redirect(w, r, "/i/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	v := url.Values{"url": {link}}
	if t := strings.TrimSpace(q.Get("title")); t != "" && !strings.Contains(t, link) {
		v.Set("title", t)
	}
	http.Redirect(w, r, "/submit?"+v.Encode(), http.StatusSeeOther)
}
