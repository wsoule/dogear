package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"dogear/internal/service"
)

type feedPage struct {
	Heading, Sub string
	Chips        []*service.CategoryNode
	Children     []*service.CategoryNode
	Tabs         bool
	Base         string // URL of this feed without sort/kind/cursor
	KindBase     string // prefix for kind filter links
	Sort, Kind   string
	Feed         feedData
}

// buildFeed loads one page of a feed and the links for the next page.
func (s *Server) buildFeed(r *http.Request, base string, fq service.FeedQuery) (service.FeedPage, feedData, error) {
	page, err := s.svc.Feed(r.Context(), viewer(r), fq)
	if err != nil {
		return page, feedData{}, err
	}
	fd := feedData{Items: page.Items, CSRF: reqCtx(r).csrf, Viewer: viewer(r)}
	if page.NextCursor != "" {
		q := url.Values{}
		if fq.Sort == "new" {
			q.Set("sort", "new")
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
	return service.FeedQuery{Sort: q.Get("sort"), Kind: q.Get("kind"), Cursor: q.Get("cursor"),
		IncludeDead: q.Get("include_dead") == "1"}
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
	fp.Sort, fp.Kind = fq.Sort, fq.Kind
	fp.Base = base
	if fp.KindBase == "" {
		fp.KindBase = base + "?kind="
		if fq.Sort == "new" {
			fp.KindBase = base + "?sort=new&kind="
		}
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
	s.serveFeed(w, r, "new", "/new", fq, feedPage{Heading: "new", Sub: "Fresh posts from people (imports excluded).", KindBase: "/new?kind="}, "new", true)
}

func (s *Server) category(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	fq := feedQuery(r)
	fq.Category = slug
	fp := feedPage{Tabs: true}
	roots, err := s.svc.Categories(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, root := range roots {
		if root.Slug == slug {
			fp.Children = root.Children
			fp.Sub = strconv.Itoa(len(root.Children)) + " lists"
		}
		for _, c := range root.Children {
			if c.Slug == slug {
				fp.Sub = "in " + root.Name
			}
		}
	}
	s.serveFeed(w, r, "", "/c/"+url.PathEscape(slug), fq, fp, slug, false)
}

func (s *Server) kind(w http.ResponseWriter, r *http.Request) {
	k := r.PathValue("kind")
	if !slices.Contains(service.Kinds, k) {
		s.notFound(w, r)
		return
	}
	fq := feedQuery(r)
	fq.Kind = k
	s.serveFeed(w, r, "", "/k/"+k, fq, feedPage{Heading: k + "s", Tabs: false}, k, false)
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
	if r.Header.Get("HX-Request") != "" && r.Header.Get("HX-Target") == "results" {
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
	Item    service.Item
	Save    saveMenuData
	Reasons []string
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
	if v := viewer(r); v != nil {
		ip.Save = s.saveMenuData(r, id, "")
	}
	desc := it.Blurb
	if desc == "" {
		desc = it.Title + " — " + it.Domain
	}
	s.render(w, r, http.StatusOK, "item", &Page{Title: it.Title, Description: desc, NoIndex: !s.svc.Indexable(r.Context(), it), Data: ip})
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
