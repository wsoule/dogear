package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"dogear/internal/loggit"
	"dogear/internal/service"
)

type previewData struct {
	Error string
	DupID int64
}

type submitPage struct {
	In      service.NewItem
	Groups  []*service.CategoryNode
	Lists   []service.List
	Error   string
	DupID   int64
	Pending bool
	Preview *previewData
}

func (s *Server) submitPageData(r *http.Request, in service.NewItem) (submitPage, error) {
	v := viewer(r)
	roots, err := s.svc.Categories(r.Context())
	if err != nil {
		return submitPage{}, err
	}
	lists, _ := s.svc.MyLists(r.Context(), v)
	return submitPage{In: in, Groups: roots, Lists: lists, Pending: v.User.TrustLevel < 1}, nil
}

// turnstileKey returns the site key when the viewer must pass Turnstile to post.
func (s *Server) turnstileKey(r *http.Request) string {
	if v := viewer(r); v != nil && v.User.TrustLevel < 1 && s.turnstile.Enabled() {
		return s.turnstile.SiteKey
	}
	return ""
}

func (s *Server) submitForm(w http.ResponseWriter, r *http.Request) {
	if viewer(r) == nil {
		s.needSignIn(w, r)
		return
	}
	q := r.URL.Query()
	// Android share targets send the URL in either "url" or "text".
	in := service.NewItem{URL: q.Get("url"), Title: q.Get("title")}
	if in.URL == "" {
		in.URL = q.Get("text")
	}
	sp, err := s.submitPageData(r, in)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "submit", &Page{Title: "post", Nav: "submit", NoIndex: true, Data: sp, TurnstileSiteKey: s.turnstileKey(r)})
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	in := service.NewItem{URL: r.FormValue("url"), Title: r.FormValue("title"), Blurb: r.FormValue("blurb"),
		Category: r.FormValue("category"), Kind: r.FormValue("kind"), Creator: r.FormValue("creator"),
		List: r.FormValue("list"), Note: r.FormValue("note")}
	fail := func(status int, msg string, dup int64) {
		sp, err := s.submitPageData(r, in)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		sp.Error, sp.DupID = msg, dup
		s.render(w, r, status, "submit", &Page{Title: "post", Nav: "submit", NoIndex: true, Data: sp, TurnstileSiteKey: s.turnstileKey(r)})
	}
	if key := s.turnstileKey(r); key != "" && !s.turnstile.Verify(r.Context(), r.FormValue("cf-turnstile-response"), s.clientIP(r)) {
		fail(http.StatusBadRequest, "Please complete the human check.", 0)
		return
	}
	it, err := s.svc.CreateItem(r.Context(), viewer(r), in)
	if err != nil {
		e, ok := service.AsError(err)
		if !ok {
			s.serverError(w, r, err)
			return
		}
		if e.Code == "unauthorized" {
			s.needSignIn(w, r)
			return
		}
		fail(statusFor(e.Code), e.Message, e.ItemID)
		return
	}
	m := "posted"
	if it.Status != "live" {
		m = "pending"
	}
	http.Redirect(w, r, "/i/"+strconv.FormatInt(it.ID, 10)+"?m="+m, http.StatusSeeOther)
}

// preview fills the title, kind, and creator fields after the URL loses
// focus, fetching the page server-side through the SSRF-safe client.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	if viewer(r) == nil {
		s.needSignIn(w, r)
		return
	}
	q := r.URL.Query()
	in := service.NewItem{URL: q.Get("url"), Title: q.Get("title"), Kind: q.Get("kind"), Creator: q.Get("creator")}
	sp := submitPage{In: in, Preview: &previewData{}}
	e, err := s.svc.Preview(r.Context(), viewer(r), in.URL)
	if err != nil {
		if se, ok := service.AsError(err); ok {
			sp.Preview.Error, sp.Preview.DupID = se.Message, se.ItemID
			status := http.StatusOK
			if r.Header.Get("HX-Request") == "" {
				status = statusFor(se.Code)
			}
			s.fragment(w, status, "submit", "autofill", sp)
			return
		}
		s.log.Warn("preview", "err", err)
	}
	if sp.In.Title == "" {
		sp.In.Title = e.Title
	}
	if sp.In.Kind == "" {
		sp.In.Kind = e.Kind
	}
	if sp.In.Creator == "" {
		sp.In.Creator = e.Creator
	}
	s.fragment(w, http.StatusOK, "submit", "autofill", sp)
}

// profilePage is /u/{user}: the archive with its tag bar, then posts.
type profilePage struct {
	P service.Profile
	// Groups are the profile's tags with their first few entries; TagCount
	// is how many tags this viewer can see.
	Groups   []service.TagGroup
	TagCount int
	// Grouping is set right after the owner asks to group untagged saves.
	Grouping bool
	Feed     feedData
	// NextArchive is the htmx URL for the next page of archive rows.
	NextArchive string
}

// archiveRows is one page of archive rows, for the "archiverows" partial.
type archiveRows struct {
	Owner   string
	Archive service.ArchiveView
	Next    string
	CSRF    string
	Viewer  *service.Actor
}

// archiveNext builds the htmx URL for the page after this one.
func archiveNext(username string, av service.ArchiveView) string {
	if av.Next == "" {
		return ""
	}
	return "/u/" + url.PathEscape(username) + "?before=" + url.QueryEscape(av.Next)
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	before := r.URL.Query().Get("before")
	if before != "" && r.Header.Get("HX-Request") != "" {
		username := r.PathValue("username")
		av, err := s.svc.UserArchive(r.Context(), viewer(r), username, before)
		if err != nil {
			s.serviceError(w, r, err)
			return
		}
		s.fragment(w, http.StatusOK, "profile", "archiverows", archiveRows{Owner: username, Archive: av,
			Next: archiveNext(username, av), CSRF: reqCtx(r).csrf, Viewer: viewer(r)})
		return
	}
	p, err := s.svc.GetProfile(r.Context(), viewer(r), r.PathValue("username"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	if before != "" {
		// No-JS paging: the full page, with the archive starting further back.
		if p.Archive, err = s.svc.Archive(r.Context(), viewer(r), p.User.ID, before); err != nil {
			s.serviceError(w, r, err)
			return
		}
	}
	nav := ""
	if p.IsOwner {
		nav = "me"
	}
	groups := s.svc.TagGroups(r.Context(), viewer(r), p.User.Username, p.Lists)
	tagCount := len(groups)
	if p.IsOwner {
		tagCount = len(p.Lists)
	}
	name := ownerName(p.User) + "'s dogear"
	desc := "Things @" + p.User.Username + " thinks are worth your time, each with a note, on Dogear."
	if p.User.Bio != "" {
		desc = p.User.Bio
	}
	share := &shareMeta{Title: name, URL: s.cfg.BaseURL + archivePath(p.User.Username),
		Image: s.cfg.BaseURL + ogArchivePath(p.User.Username), ImageAlt: name + ", with the newest things saved in it",
		Feed: s.cfg.BaseURL + archivePath(p.User.Username) + "/feed.xml"}
	// Profiles are never indexed: they would otherwise be the main target of SEO spam.
	s.render(w, r, http.StatusOK, "profile", &Page{Title: "@" + p.User.Username, Description: desc, Nav: nav, NoIndex: true, Share: share,
		Data: profilePage{P: p, NextArchive: archiveNext(p.User.Username, p.Archive),
			Groups: groups, TagCount: tagCount, Grouping: p.IsOwner && r.URL.Query().Has("grouping"),
			Feed: feedData{Items: p.Posts, CSRF: reqCtx(r).csrf, Viewer: viewer(r), Compact: compactView(r)}}})
}

func (s *Server) block(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if err := s.svc.Block(r.Context(), viewer(r), username, r.FormValue("block") == "1"); err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+url.PathEscape(username)+"?m=done", http.StatusSeeOther)
}

type listPage struct {
	L         service.ListView
	Sort      string
	SortLinks []tabLink
	// loggit sync panel (owner only)
	LoggitEnabled, LoggitConnected bool
	Link                           *service.ListLink
	LoggitLists                    []loggit.List
	LoggitError                    string
	Badges                         map[int64]string
	Embed                          string // iframe snippet, public tags only
}

var listSortLabels = map[string]string{"owner": "their order", "top": "top", "added": "recently added", "az": "a–z"}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	lv, err := s.svc.GetList(r.Context(), viewer(r), r.PathValue("username"), r.PathValue("slug"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	sort := r.URL.Query().Get("sort")
	if !slices.Contains(service.ListSorts, sort) {
		sort = "owner"
	}
	service.SortEntries(lv.Entries, sort)
	lp := listPage{L: lv, Sort: sort, Badges: map[int64]string{}}
	if lv.IsOwner && s.svc.LoggitEnabled() {
		lp.LoggitEnabled = true
		lp.LoggitConnected = s.svc.LoggitAccount(r.Context(), viewer(r).ID()) != nil
		lp.Link = s.svc.ListLinkFor(r.Context(), lv.ID)
		if lp.Link != nil {
			ids := make([]int64, len(lv.Entries))
			for i, e := range lv.Entries {
				ids[i] = e.Item.ID
			}
			for id := range s.svc.UnmappedItems(r.Context(), ids) {
				lp.Badges[id] = "not on loggit"
			}
		} else if lp.LoggitConnected {
			ls, err := s.svc.LoggitLists(r.Context(), viewer(r))
			if err != nil {
				lp.LoggitError = "Couldn't load your loggit lists: " + err.Error()
			}
			lp.LoggitLists = ls
		}
	}
	base := "/u/" + url.PathEscape(lv.Owner) + "/" + url.PathEscape(lv.Slug)
	for _, k := range service.ListSorts {
		u := base
		if k != "owner" {
			u += "?sort=" + k
		}
		lp.SortLinks = append(lp.SortLinks, tabLink{Key: k, Label: listSortLabels[k], URL: u})
	}
	pg := &Page{Title: lv.Title + " · @" + lv.Owner, Description: lv.Description, NoIndex: !lv.Indexable}
	// Only public tags get a preview card, feed, and embed: unlisted ones are
	// link-only and shouldn't be spread further by those surfaces.
	if lv.Visibility == "public" {
		if pg.Description == "" {
			pg.Description = things(len(lv.Entries)) + " @" + lv.Owner + " tagged #" + lv.Slug + " on Dogear."
		}
		pg.Share = &shareMeta{Title: lv.Title + " · @" + lv.Owner, URL: s.cfg.BaseURL + base, Image: s.cfg.BaseURL + ogTagPath(lv.Owner, lv.Slug),
			ImageAlt: lv.Title + " by @" + lv.Owner + ", with the first things in it", Feed: s.cfg.BaseURL + base + "/feed.xml"}
		lp.Embed = s.embedSnippet(lv)
	}
	pg.Data = lp
	s.render(w, r, http.StatusOK, "list", pg)
}

func (s *Server) createList(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	l, err := s.svc.CreateList(r.Context(), v, service.ListInput{Title: r.FormValue("title"),
		Description: r.FormValue("description"), Visibility: r.FormValue("visibility")})
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+v.User.Username+"/"+l.Slug+"?m=list", http.StatusSeeOther)
}

func (s *Server) editList(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	slug := r.PathValue("slug")
	if r.FormValue("op") == "delete" {
		if err := s.svc.DeleteList(r.Context(), v, slug); err != nil {
			s.serviceError(w, r, err)
			return
		}
		http.Redirect(w, r, "/u/"+v.User.Username+"?m=listdel", http.StatusSeeOther)
		return
	}
	title, desc, vis := r.FormValue("title"), r.FormValue("description"), r.FormValue("visibility")
	l, err := s.svc.UpdateList(r.Context(), v, slug, service.ListPatch{Title: &title, Description: &desc, Visibility: &vis})
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+v.User.Username+"/"+l.Slug+"?m=saved", http.StatusSeeOther)
}

// editEntry updates the note, moves an entry up or down in a tag, or
// untags it.
func (s *Server) editEntry(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	slug := r.PathValue("slug")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	switch r.FormValue("op") {
	case "remove":
		err = s.svc.RemoveFromList(ctx, v, slug, id)
	case "up", "down":
		var lv service.ListView
		lv, err = s.svc.GetList(ctx, v, v.User.Username, slug)
		if err == nil {
			order := make([]int64, len(lv.Entries))
			idx := -1
			for i, e := range lv.Entries {
				order[i] = e.Item.ID
				if e.Item.ID == id {
					idx = i
				}
			}
			j := idx - 1
			if r.FormValue("op") == "down" {
				j = idx + 1
			}
			if idx >= 0 && j >= 0 && j < len(order) {
				order[idx], order[j] = order[j], order[idx]
				_, err = s.svc.UpdateList(ctx, v, slug, service.ListPatch{ItemOrder: order})
			}
		}
	default:
		// The note lives on the save, so editing it here changes it on
		// every tag; an empty note clears it.
		note := r.FormValue("note")
		err = s.svc.SaveItem(ctx, v, id, &note)
	}
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+v.User.Username+"/"+slug, http.StatusSeeOther)
}
