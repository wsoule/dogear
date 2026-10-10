package web

import (
	"errors"
	"fmt"
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
	Chips        []*service.CategoryNode
	Children     []*service.CategoryNode
	Tabs         bool
	Base         string // URL of this feed without sort/kind/cursor
	Sort, Kind   string
	Here         string // current URL, for the layout toggle's return link
	SortLinks    []tabLink
	KindLinks    []tabLink
	Crumbs       []tabLink // breadcrumb trail above the heading
	Source       *db.SourceListByCategoryRow
	Feed         feedData
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
	chips, err := s.svc.TopCategories(r.Context(), 12)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	fq := feedQuery(r)
	nav := "top"
	if fq.Sort == "new" {
		nav = "new"
	}
	s.serveFeed(w, r, nav, "/", fq, feedPage{Chips: chips, Tabs: true}, "", false)
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
	s.render(w, r, http.StatusOK, "static_pages", &Page{Title: "categories", Page: "categories", Data: roots})
}

type searchPage struct {
	Q     string
	Total string
	Feed  feedData
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
	}
	if t := r.Header.Get("HX-Target"); r.Header.Get("HX-Request") != "" && (t == "results" || t == "cmdk-results") {
		if t == "cmdk-results" {
			// The ⌘K dialog shows a short, compact result list.
			if len(sp.Feed.Items) > 8 {
				sp.Feed.Items = sp.Feed.Items[:8]
			}
			sp.Feed.Next, sp.Feed.NextPage, sp.Feed.Compact = "", "", true
		}
		s.fragment(w, http.StatusOK, "search", "results", sp)
		return
	}
	s.render(w, r, http.StatusOK, "search", &Page{Title: "search", Nav: "search", NoIndex: true, Data: sp})
}

type saveMenuData struct {
	ItemID int64
	Lists  []service.List
	Saved  []string
	Error  string
	CSRF   string
}

type itemPage struct {
	Item       service.Item
	ParentName string
	Similar    feedData // closest items by meaning
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
	dg := &dogearState{ItemID: it.ID, CSRF: reqCtx(r).csrf}
	for _, slug := range ip.Save.Saved {
		if slug == service.DogearSlug {
			dg.On = true
		}
	}
	s.render(w, r, http.StatusOK, "item", &Page{Title: it.Title, Description: desc, NoIndex: !s.svc.Indexable(r.Context(), it), Data: ip, Dogear: dg})
}

func (s *Server) saveMenuData(r *http.Request, itemID int64, errMsg string) saveMenuData {
	v := viewer(r)
	d := saveMenuData{ItemID: itemID, Error: errMsg, CSRF: reqCtx(r).csrf}
	d.Lists, _ = s.svc.MyLists(r.Context(), v)
	d.Saved, _ = s.svc.SavedIn(r.Context(), v, itemID)
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
		s.fragment(w, http.StatusOK, "feed", "vote", voteData{ItemID: id, Likes: st.Likes, MyVote: st.MyVote, CSRF: reqCtx(r).csrf})
		return
	}
	s.back(w, r, "/i/"+strconv.FormatInt(id, 10))
}

// dogear toggles the item in the viewer's Dogeared list (the corner mark).
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
		s.fragment(w, http.StatusOK, "item", "dogearbtn", dogearState{ItemID: id, On: on, CSRF: reqCtx(r).csrf})
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
	v := viewer(r)
	slug := r.FormValue("list")
	if slug == "" {
		title := r.FormValue("new_list")
		if title == "" {
			title = "Favorites"
		}
		l, err := s.svc.CreateList(r.Context(), v, service.ListInput{Title: title})
		if err != nil {
			s.saveError(w, r, id, err)
			return
		}
		slug = l.Slug
	}
	if err := s.svc.SaveToList(r.Context(), v, slug, id, r.FormValue("note"), nil); err != nil {
		s.saveError(w, r, id, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		s.fragment(w, http.StatusOK, "item", "savemenu", s.saveMenuData(r, id, ""))
		return
	}
	http.Redirect(w, r, "/i/"+strconv.FormatInt(id, 10)+"?m=saved#save", http.StatusSeeOther)
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

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
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
