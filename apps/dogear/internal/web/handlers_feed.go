package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/service"
)

const viewCookie = "view"

// compactView reports whether the visitor chose the dense one-line layout.
func compactView(r *http.Request) bool {
	c, err := r.Cookie(viewCookie)
	return err == nil && c.Value == "compact"
}

// setView stores the feed layout preference in a cookie and returns to the
// page the toggle was clicked on (same-site paths only).
func (s *Server) setView(w http.ResponseWriter, r *http.Request) {
	mode := r.PathValue("mode")
	if mode != "compact" && mode != "rows" {
		s.notFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: viewCookie, Value: mode, Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther)
}

// localPath returns next only when it is a plain same-site path, else "/".
// Browsers drop tabs and newlines and treat backslashes as slashes, so
// "/\t/evil.com" or "/\\evil.com" could otherwise become "//evil.com".
func localPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || next == "" || next[0] != '/' || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\t\r\n") || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

type listsPage struct {
	service.ListIndex
	Sort      string
	SortLinks []tabLink
}

func (s *Server) lists(w http.ResponseWriter, r *http.Request) {
	idx, err := s.svc.ListsIndex(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sort := r.URL.Query().Get("sort")
	if !slices.Contains(service.IndexSorts, sort) {
		sort = "stars"
	}
	lp := listsPage{ListIndex: service.SortIndex(idx, sort), Sort: sort}
	for _, k := range service.IndexSorts {
		u := "/lists"
		if k != "stars" {
			u += "?sort=" + k
		}
		lp.SortLinks = append(lp.SortLinks, tabLink{Key: k, Label: k, URL: u})
	}
	s.render(w, r, http.StatusOK, "lists", &Page{Title: "lists", Nav: "lists", Data: lp,
		Description: "People's lists of things they think are cool, and every awesome list Dogear was seeded from."})
}

type tabLink struct{ Key, Label, URL string }

var sortLabels = map[string]string{"top": "top", "new": "new", "stars": "stars", "az": "a–z"}

// feedURL builds a feed link that keeps the other filter: changing the sort
// keeps the kind and vice versa.
// commas formats an integer with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func feedURL(base, sort, kind string) string {
	q := url.Values{}
	if sort != "" && sort != "top" {
		q.Set("sort", sort)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

type feedPage struct {
	Heading, Sub string
	// Topics are the any-medium topic chips; TopicHere marks the current one.
	Topics     []service.Topic
	TopicHere  string
	Children   []*service.CategoryNode
	Tabs       bool
	Base       string // URL of this feed without sort/kind/cursor
	Sort, Kind string
	Here       string // current URL, for the layout toggle's return link
	SortLinks  []tabLink
	KindLinks  []tabLink
	Intro      bool      // show the one-line explanation (home, signed out)
	Crumbs     []tabLink // breadcrumb trail above the heading
	Source     *db.SourceListByCategoryRow
	Feed       feedData
	Recent     []service.RecentSave // home only: the "recently saved" strip
}

// buildFeed loads one page of a feed and the links for the next page.
func (s *Server) buildFeed(r *http.Request, base string, fq service.FeedQuery) (service.FeedPage, feedData, error) {
	page, err := s.svc.Feed(r.Context(), viewer(r), fq)
	if err != nil {
		return page, feedData{}, err
	}
	fd := feedData{Items: page.Items, CSRF: reqCtx(r).csrf, Viewer: viewer(r), Compact: compactView(r),
		PrefetchAt: max(0, len(page.Items)-12)}
	if page.NextCursor != "" {
		q := url.Values{}
		if fq.Sort != "" && fq.Sort != "top" {
			q.Set("sort", fq.Sort)
		}
		if fq.Kind != "" {
			q.Set("kind", fq.Kind)
		}
		if fq.Q != "" {
			q.Set("q", fq.Q)
		}
		q.Set("cursor", page.NextCursor)
		fd.NextPage = base + "?" + q.Encode()
		q.Set("partial", "1")
		fd.Next = base + "?" + q.Encode()
	}
	return page, fd, nil
}

// feedQuery reads sort, kind, and cursor from the URL.
func feedQuery(r *http.Request) service.FeedQuery {
	q := r.URL.Query()
	fq := service.FeedQuery{Sort: q.Get("sort"), Kind: q.Get("kind"), Cursor: q.Get("cursor"),
		IncludeDead: q.Get("include_dead") == "1"}
	if fq.Cursor != "" {
		fq.Limit = 50
	}
	return fq
}

// serveFeed renders a feed page, or only the next rows for infinite scroll.
func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request, nav, base string, fq service.FeedQuery, fp feedPage, title string, noindex bool) {
	page, fd, err := s.buildFeed(r, base, fq)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	if r.URL.Query().Get("partial") == "1" {
		s.fragment(w, http.StatusOK, "feed", "rows", fd)
		return
	}
	fp.Feed = fd
	fp.Here = r.URL.RequestURI()
	fp.Sort, fp.Kind = fq.Sort, fq.Kind
	if fp.Sort == "" {
		fp.Sort = "top"
	}
	fp.Base = base
	for _, k := range service.FeedSorts {
		fp.SortLinks = append(fp.SortLinks, tabLink{Key: k, Label: sortLabels[k], URL: feedURL(base, k, fq.Kind)})
	}
	fp.KindLinks = append(fp.KindLinks, tabLink{Key: "", Label: "all", URL: feedURL(base, fq.Sort, "")})
	for _, k := range service.Kinds {
		fp.KindLinks = append(fp.KindLinks, tabLink{Key: k, Label: k, URL: feedURL(base, fq.Sort, k)})
	}
	if fp.Heading == "" && page.Category != nil {
		fp.Heading = page.Category.Name
	}
	s.render(w, r, http.StatusOK, "feed", &Page{Title: title, Nav: nav, NoIndex: noindex || fq.Cursor != "", Data: fp,
		Description: "Things people think are cool, each with a one-line reason."})
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	topics, err := s.svc.Topics(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	fq := feedQuery(r)
	fq.MixKinds = true // the unfiltered top feed mixes in videos, articles, channels
	nav := "top"
	if fq.Sort == "new" {
		nav = "new"
	}
	fp := feedPage{Topics: topics, Tabs: true, Intro: fq.Cursor == ""}
	if fq.Cursor == "" && r.URL.Query().Get("partial") != "1" {
		// The strip is a nice-to-have; a failure only hides it.
		if fp.Recent, err = s.svc.RecentSaves(r.Context(), viewer(r), recentStripSize); err != nil {
			s.log.Warn("recent saves", "err", err)
		}
	}
	s.serveFeed(w, r, nav, "/", fq, fp, "", false)
}

// topic is /t/{slug}: everything filed under one broad topic, any medium.
func (s *Server) topic(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	topics, err := s.svc.Topics(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	i := slices.IndexFunc(topics, func(t service.Topic) bool { return t.Slug == slug })
	if i < 0 {
		s.notFound(w, r)
		return
	}
	fq := feedQuery(r)
	fq.Topic = slug
	fp := feedPage{Heading: topics[i].Name, Sub: topics[i].Description, Topics: topics, TopicHere: slug, Tabs: true}
	s.serveFeed(w, r, "", "/t/"+url.PathEscape(slug), fq, fp, topics[i].Name, false)
}

// categoriesPage is /categories: the broad topics, then the category tree.
type categoriesPage struct {
	Topics []service.Topic
	Roots  []*service.CategoryNode
}

func (s *Server) newFeed(w http.ResponseWriter, r *http.Request) {
	fq := feedQuery(r)
	fq.Sort = "new"
	s.serveFeed(w, r, "", "/new", fq, feedPage{Heading: "new", Sub: "Fresh posts from people, newest first."}, "new", false)
}

func (s *Server) category(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	fq := feedQuery(r)
	fq.Category = slug
	fp := feedPage{Tabs: true, Crumbs: []tabLink{{Label: "categories", URL: "/categories"}}}
	roots, err := s.svc.Categories(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, root := range roots {
		if root.Slug == slug {
			fp.Children = root.Children
			fp.Sub = fmt.Sprintf("%s items in %d lists", commas(root.Count), len(root.Children))
		}
		for _, c := range root.Children {
			if c.Slug == slug {
				fp.Crumbs = append(fp.Crumbs, tabLink{Label: root.Name, URL: "/c/" + root.Slug})
				fp.Sub = commas(c.Count) + " items"
			}
		}
	}
	fp.Source = s.svc.SourceForCategory(r.Context(), slug)
	s.serveFeed(w, r, "", "/c/"+url.PathEscape(slug), fq, fp, slug, false)
}

// kind redirects /k/{kind} to the home feed filtered by that kind, so every
// feed shares one set of controls.
func (s *Server) kind(w http.ResponseWriter, r *http.Request) {
	k := r.PathValue("kind")
	if !slices.Contains(service.Kinds, k) {
		s.notFound(w, r)
		return
	}
	http.Redirect(w, r, feedURL("/", r.URL.Query().Get("sort"), k), http.StatusMovedPermanently)
}

func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	roots, err := s.svc.Categories(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	topics, err := s.svc.Topics(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "static_pages", &Page{Title: "categories", Page: "categories", Data: categoriesPage{Topics: topics, Roots: roots}})
}

type searchPage struct {
	Q     string
	Total string
	Feed  feedData
	// Creators are channels and people for the query, shown above results.
	Creators feedData
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	sp := searchPage{Q: q, Total: "thousands of"}
	if q != "" {
		fq := feedQuery(r)
		fq.Q = q
		_, fd, err := s.buildFeed(r, "/search", fq)
		if err != nil {
			s.serviceError(w, r, err)
			return
		}
		sp.Feed = fd
		if r.URL.Query().Get("partial") == "1" {
			s.fragment(w, http.StatusOK, "feed", "rows", fd)
			return
		}
		if cs, err := s.svc.SearchCreators(r.Context(), viewer(r), q, fd.Items, 6); err == nil && len(cs) > 0 {
			sp.Creators = feedData{Items: cs, CSRF: fd.CSRF, Viewer: fd.Viewer, Compact: true}
			// Don't list a creator twice: drop the strip's entries from the
			// results below it.
			sp.Feed.Items = slices.DeleteFunc(slices.Clone(fd.Items), func(it service.Item) bool {
				return slices.ContainsFunc(cs, func(c service.Item) bool { return c.ID == it.ID })
			})
		}
	}
	if t := r.Header.Get("HX-Target"); r.Header.Get("HX-Request") != "" && (t == "results" || t == "cmdk-results") {
		if t == "cmdk-results" {
			// The ⌘K dialog shows a short, compact result list.
			if len(sp.Feed.Items) > 8 {
				sp.Feed.Items = sp.Feed.Items[:8]
			}
			if len(sp.Creators.Items) > 3 {
				sp.Creators.Items = sp.Creators.Items[:3]
			}
			sp.Feed.Next, sp.Feed.NextPage, sp.Feed.Compact = "", "", true
		}
		s.fragment(w, http.StatusOK, "search", "results", sp)
		return
	}
	s.render(w, r, http.StatusOK, "search", &Page{Title: "search", Nav: "search", NoIndex: true, Data: sp})
}

// saveMenuData drives the "dogear & tag" menu on item pages: the viewer's
// tags (Lists), which of them hold the item (Saved), and their note on it.
type saveMenuData struct {
	ItemID   int64
	Lists    []service.List
	Saved    []string
	Note     string
	Dogeared bool
	Error    string
	CSRF     string
}

type itemPage struct {
	Item       service.Item
	ParentName string
	Similar    feedData // closest items by meaning
	Works      feedData // a channel's or person's works, on their own page
	Topics     []service.Topic
	Save       saveMenuData
	Reasons    []string
}

func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	it, err := s.svc.GetItem(r.Context(), viewer(r), id)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	ip := itemPage{Item: it, Reasons: []string{"spam", "dead", "offensive", "duplicate", "other"}}
	if sim, err := s.svc.Similar(r.Context(), viewer(r), it.ID, 6); err == nil && len(sim) > 0 {
		ip.Similar = feedData{Items: sim, CSRF: reqCtx(r).csrf, Viewer: viewer(r), Compact: true}
	}
	ip.Topics = s.svc.ItemTopics(r.Context(), it.ID)
	if it.IsCreator() {
		if works, err := s.svc.CreatorWorks(r.Context(), viewer(r), it.ID, 50); err == nil && len(works) > 0 {
			ip.Works = feedData{Items: works, CSRF: reqCtx(r).csrf, Viewer: viewer(r), Compact: true}
		}
	}
	if it.Category != nil && it.Category.Parent != "" {
		ip.ParentName = it.Category.Parent
		if roots, err := s.svc.Categories(r.Context()); err == nil {
			for _, root := range roots {
				if root.Slug == it.Category.Parent {
					ip.ParentName = root.Name
				}
			}
		}
	}
	if v := viewer(r); v != nil {
		ip.Save = s.saveMenuData(r, id, "")
	}
	desc := it.Blurb
	if desc == "" {
		desc = it.Title + " — " + it.Domain
	}
	dg := &dogearState{ItemID: it.ID, On: it.Dogeared, Tags: it.TagCount, CSRF: reqCtx(r).csrf}
	s.render(w, r, http.StatusOK, "item", &Page{Title: it.Title, Description: desc, NoIndex: !s.svc.Indexable(r.Context(), it), Data: ip, Dogear: dg})
}

func (s *Server) saveMenuData(r *http.Request, itemID int64, errMsg string) saveMenuData {
	v := viewer(r)
	d := saveMenuData{ItemID: itemID, Error: errMsg, CSRF: reqCtx(r).csrf}
	d.Lists, _ = s.svc.MyLists(r.Context(), v)
	d.Saved, _ = s.svc.SavedIn(r.Context(), v, itemID)
	d.Note, d.Dogeared = s.svc.SavedNote(r.Context(), v, itemID)
	return d
}

func (s *Server) vote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	value := 0
	if r.Method != http.MethodDelete {
		value, _ = strconv.Atoi(r.FormValue("value"))
	}
	st, err := s.svc.Vote(r.Context(), viewer(r), id, value)
	if err != nil {
		if r.Header.Get("HX-Request") != "" {
			if e, ok := service.AsError(err); ok {
				w.Header().Set("HX-Reswap", "none")
				http.Error(w, e.Message, statusFor(e.Code))
				return
			}
		}
		s.serviceError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		name := "vote"
		if r.URL.Query().Get("ui") == "like" {
			name = "like"
		}
		s.fragment(w, http.StatusOK, "feed", name, voteData{ItemID: id, Likes: st.Likes, MyVote: st.MyVote, CSRF: reqCtx(r).csrf})
		return
	}
	s.back(w, r, "/i/"+strconv.FormatInt(id, 10))
}

// dogear saves the item to the viewer's archive, or unsaves it (the corner
// mark and the inline button).
func (s *Server) dogear(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	on, err := s.svc.ToggleDogear(r.Context(), viewer(r), id)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		st := dogearState{ItemID: id, On: on, CSRF: reqCtx(r).csrf}
		// Swap the control that was tapped; on an item page, also update the
		// other one (the corner or the inline button) out of band.
		onItemPage := strings.Contains(r.Header.Get("HX-Current-URL"), "/i/"+strconv.FormatInt(id, 10))
		primary, other := "dogearbtn", "dogearinline"
		if r.URL.Query().Get("ui") == "inline" {
			primary, other = "dogearinline", "dogearbtn"
		}
		var buf strings.Builder
		if err := s.tmpl["item"].ExecuteTemplate(&buf, primary, st); err != nil {
			s.serverError(w, r, err)
			return
		}
		if onItemPage {
			var oob strings.Builder
			if err := s.tmpl["item"].ExecuteTemplate(&oob, other, st); err == nil {
				buf.WriteString(strings.Replace(oob.String(), "<form ", `<form hx-swap-oob="true" `, 1))
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, buf.String())
		return
	}
	http.Redirect(w, r, "/i/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) saveMenu(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if viewer(r) == nil {
		s.needSignIn(w, r)
		return
	}
	s.fragment(w, http.StatusOK, "item", "savemenu", s.saveMenuData(r, id, ""))
}

func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.applySave(r, id); err != nil {
		s.saveError(w, r, id, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		// Saving dogears the item, so refresh both dog-ear controls and the
		// menu's summary line out of band along with the menu.
		d := s.saveMenuData(r, id, "")
		st := dogearState{ItemID: id, On: d.Dogeared, Tags: len(d.Saved), CSRF: d.CSRF}
		var buf strings.Builder
		if err := s.tmpl["item"].ExecuteTemplate(&buf, "savemenu", d); err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, name := range []string{"dogearbtn", "dogearinline"} {
			var oob strings.Builder
			if err := s.tmpl["item"].ExecuteTemplate(&oob, name, st); err == nil {
				buf.WriteString(strings.Replace(oob.String(), "<form ", `<form hx-swap-oob="true" `, 1))
			}
		}
		var sum strings.Builder
		if err := s.tmpl["item"].ExecuteTemplate(&sum, "savesummary", d); err == nil {
			buf.WriteString(strings.Replace(sum.String(), "<summary ", `<summary hx-swap-oob="true" `, 1))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, buf.String())
		return
	}
	http.Redirect(w, r, "/i/"+strconv.FormatInt(id, 10)+"?m=saved#save", http.StatusSeeOther)
}

// applySave saves the item to the viewer's archive and makes their tags
// match the menu. The checklist form (menu=1) sends every tag that should
// hold the item, so unchecked tags lose it, and its note field (prefilled
// with the current note) replaces the note. Older callers send a single
// "list" and only add, and their note only fills in a missing one. A typed
// new tag name creates that tag too.
func (s *Server) applySave(r *http.Request, id int64) error {
	ctx, v := r.Context(), viewer(r)
	if err := r.ParseForm(); err != nil {
		return &service.Error{Code: "invalid", Message: "bad form"}
	}
	want := r.Form["lists"]
	if l := r.FormValue("list"); l != "" {
		want = append(want, l)
	}
	if title := strings.TrimSpace(r.FormValue("new_list")); title != "" {
		l, err := s.svc.CreateList(ctx, v, service.ListInput{Title: title})
		if err != nil {
			return err
		}
		want = append(want, l.Slug)
	}
	menu := r.FormValue("menu") == "1"
	if len(want) == 0 && !menu {
		return &service.Error{Code: "invalid", Message: "pick a tag or name a new one"}
	}
	// Tag before saving the note, so an item filed only under a private tag
	// is never briefly an untagged (public) save.
	have, err := s.svc.SavedIn(ctx, v, id)
	if err != nil {
		return err
	}
	for _, slug := range want {
		if !slices.Contains(have, slug) {
			if err := s.svc.SaveToList(ctx, v, slug, id, "", nil); err != nil {
				return err
			}
		}
	}
	if menu {
		note := r.FormValue("note")
		if err := s.svc.SaveItem(ctx, v, id, &note); err != nil {
			return err
		}
	} else if old, _ := s.svc.SavedNote(ctx, v, id); old == "" && r.FormValue("note") != "" {
		note := r.FormValue("note")
		if err := s.svc.SaveItem(ctx, v, id, &note); err != nil {
			return err
		}
	}
	if menu {
		for _, slug := range have {
			if !slices.Contains(want, slug) {
				if err := s.svc.RemoveFromList(ctx, v, slug, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) saveError(w http.ResponseWriter, r *http.Request, id int64, err error) {
	e, ok := service.AsError(err)
	if !ok {
		s.serverError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		s.fragment(w, http.StatusOK, "item", "savemenu", s.saveMenuData(r, id, e.Message))
		return
	}
	s.errorPage(w, r, statusFor(e.Code), e.Message)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.svc.Report(r.Context(), viewer(r), id, r.FormValue("reason"), r.FormValue("note")); err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/i/"+strconv.FormatInt(id, 10)+"?m=reported", http.StatusSeeOther)
}

// back redirects to the Referer when it is on this site, else to fallback.
func (s *Server) back(w http.ResponseWriter, r *http.Request, fallback string) {
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Path != "" && (ref.Host == "" || ref.Host == r.Host) {
		http.Redirect(w, r, ref.RequestURI(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}

func statusFor(code string) int {
	switch code {
	case "invalid":
		return http.StatusBadRequest
	case "unauthorized":
		return http.StatusUnauthorized
	case "forbidden":
		return http.StatusForbidden
	case "not_found":
		return http.StatusNotFound
	case "duplicate":
		return http.StatusConflict
	case "rate_limited":
		return http.StatusTooManyRequests
	case "rejected":
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

type errorData struct {
	Code    int
	Message string
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "static_pages", &Page{Title: http.StatusText(status), Page: "error", NoIndex: true,
		Data: errorData{Code: status, Message: msg}})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.errorPage(w, r, http.StatusNotFound, "Nothing here. It may have been removed, or it's private.")
}

// statusClientClosed is nginx's "client closed request" code. Nobody reads
// the response; it only keeps request logs from counting these as 500s.
const statusClientClosed = 499

// serverError logs an unexpected handler error and shows the 500 page. When
// the client already went away (an htmx live search replacing its previous
// request, a closed tab), the query fails with context.Canceled. That is not
// a server fault, so it logs at info and answers 499 with no body.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
		s.log.Info("client went away", "path", r.URL.Path, "err", err)
		w.WriteHeader(statusClientClosed)
		return
	}
	s.log.Error("handler", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "Something broke on our side. Try again in a moment.")
}

// serviceError renders a user-facing service error or a 500.
func (s *Server) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	var e *service.Error
	if !errors.As(err, &e) {
		s.serverError(w, r, err)
		return
	}
	if e.Code == "unauthorized" {
		s.needSignIn(w, r)
		return
	}
	if e.Code == "not_found" {
		s.notFound(w, r)
		return
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Seconds())+1))
	}
	s.errorPage(w, r, statusFor(e.Code), e.Message)
}

func (s *Server) staticPage(name, page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.render(w, r, http.StatusOK, "static_pages", &Page{Title: name, Page: page})
	}
}
