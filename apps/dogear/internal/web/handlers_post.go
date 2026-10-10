package web

import (
	"net/http"
	"net/url"
	"strconv"

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

type profilePage struct {
	P    service.Profile
	Feed feedData
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.GetProfile(r.Context(), viewer(r), r.PathValue("username"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	nav := ""
	if p.IsOwner {
		nav = "me"
	}
	// Profiles are never indexed: they would otherwise be the main target of SEO spam.
	s.render(w, r, http.StatusOK, "profile", &Page{Title: "@" + p.User.Username, Nav: nav, NoIndex: true,
		Data: profilePage{P: p, Feed: feedData{Items: p.Posts, CSRF: reqCtx(r).csrf, Viewer: viewer(r), Compact: compactView(r)}}})
}

func (s *Server) block(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if err := s.svc.Block(r.Context(), viewer(r), username, r.FormValue("block") == "1"); err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+url.PathEscape(username)+"?m=done", http.StatusSeeOther)
}

type listPage struct{ L service.ListView }

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	lv, err := s.svc.GetList(r.Context(), viewer(r), r.PathValue("username"), r.PathValue("slug"))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "list", &Page{Title: lv.Title + " · @" + lv.Owner, Description: lv.Description,
		NoIndex: !lv.Indexable, Data: listPage{L: lv}})
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

// editEntry updates a note, moves an entry up or down, or removes it.
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
		var lv service.ListView
		lv, err = s.svc.GetList(ctx, v, v.User.Username, slug)
		if err == nil {
			pos := 0
			for _, e := range lv.Entries {
				if e.Item.ID == id {
					pos = e.Position
				}
			}
			err = s.svc.SaveToList(ctx, v, slug, id, r.FormValue("note"), &pos)
		}
	}
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/u/"+v.User.Username+"/"+slug, http.StatusSeeOther)
}
