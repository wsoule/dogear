package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dogear/internal/importfile"
	"dogear/internal/service"
)

// withNote is an item object with the untrusted-content note alongside it.
type withNote struct {
	service.Item
	Note string `json:"note"`
}

func (a *API) categories(w http.ResponseWriter, r *http.Request) {
	roots, err := a.svc.Categories(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]*service.CategoryNode, 0, len(roots))
	for _, c := range roots {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": out})
}

func (a *API) topics(w http.ResponseWriter, r *http.Request) {
	ts, err := a.svc.Topics(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"topics": ts})
}

func (a *API) listItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := a.svc.Feed(r.Context(), a.optionalActor(r), service.FeedQuery{
		Category: q.Get("category"), Topic: q.Get("topic"), Kind: q.Get("kind"), Sort: q.Get("sort"), Q: q.Get("q"),
		Cursor: q.Get("cursor"), Limit: limit, IncludeDead: q.Get("include_dead") == "1",
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page.Items, "next_cursor": page.NextCursor, "note": service.InjectionNote})
}

func (a *API) getItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	it, err := a.svc.GetItem(r.Context(), a.optionalActor(r), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withNote{Item: it, Note: service.InjectionNote})
}

func (a *API) createItem(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	var in service.NewItem
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	it, err := a.svc.CreateItem(r.Context(), act, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := http.StatusCreated
	if it.Status != "live" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, withNote{Item: it, Note: service.InjectionNote})
}

func (a *API) vote(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Value int `json:"value"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Value != 1 && body.Value != -1 {
		writeErr(w, &service.Error{Code: "invalid", Message: "value must be 1 or -1"})
		return
	}
	st, err := a.svc.Vote(r.Context(), act, id, body.Value)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) unvote(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	st, err := a.svc.Vote(r.Context(), act, id, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) report(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.svc.Report(r.Context(), act, id, body.Reason, body.Note); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type publicUser struct {
	Username    string         `json:"username"`
	DisplayName string         `json:"display_name,omitempty"`
	Bio         string         `json:"bio"`
	Website     string         `json:"website,omitempty"`
	CreatedAt   string         `json:"created_at"`
	Lists       []service.List `json:"lists"`
	Followers   int            `json:"followers"`
	Following   int            `json:"following"`
	// FollowedByMe is whether the signed-in caller follows this user.
	FollowedByMe bool `json:"followed_by_me"`
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.GetProfile(r.Context(), a.optionalActor(r), r.PathValue("username"))
	if err != nil {
		writeErr(w, err)
		return
	}
	u := publicUser{Username: p.User.Username, Bio: p.User.Bio, CreatedAt: p.User.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), Lists: []service.List{},
		Followers: p.FollowerCount, Following: p.FollowingCount, FollowedByMe: p.ViewerFollows}
	if p.User.DisplayName != nil {
		u.DisplayName = *p.User.DisplayName
	}
	if p.User.Website != nil {
		u.Website = *p.User.Website
	}
	for _, l := range p.Lists {
		if l.Visibility == "public" {
			u.Lists = append(u.Lists, l)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "note": service.InjectionNote})
}

func (a *API) getList(w http.ResponseWriter, r *http.Request) {
	lv, err := a.svc.GetList(r.Context(), a.optionalActor(r), r.PathValue("username"), r.PathValue("slug"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": lv, "note": service.InjectionNote})
}

func (a *API) me(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	u := act.User
	writeJSON(w, http.StatusOK, map[string]any{
		"username": u.Username, "display_name": u.DisplayName, "bio": u.Bio, "website": u.Website,
		"trust_level": u.TrustLevel, "role": u.Role,
		"rate_limit": map[string]any{"posts_remaining": a.svc.PostRemaining(act)},
		"scopes":     act.Scopes,
	})
}

func (a *API) patchMe(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	var in service.ProfileInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	u, err := a.svc.UpdateProfile(r.Context(), act.User, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "display_name": u.DisplayName, "bio": u.Bio, "website": u.Website})
}

func (a *API) deleteMe(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	var body struct {
		Confirm     string `json:"confirm"`
		DeletePosts bool   `json:"delete_posts"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := a.svc.DeleteAccount(r.Context(), act, body.Confirm, body.DeletePosts); err != nil {
		writeErr(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(a.baseURL, "https://")})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *API) myItems(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	items, err := a.svc.UserItems(r.Context(), act, act.ID(), 200)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "note": service.InjectionNote})
}

func (a *API) export(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	data, err := a.svc.Export(r.Context(), act)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (a *API) myLists(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	lists, err := a.svc.MyLists(r.Context(), act)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": lists})
}

func (a *API) createList(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	var in service.ListInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	l, err := a.svc.CreateList(r.Context(), act, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"list": l})
}

func (a *API) patchList(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	var p service.ListPatch
	if err := decode(r, &p); err != nil {
		writeErr(w, err)
		return
	}
	l, err := a.svc.UpdateList(r.Context(), act, r.PathValue("slug"), p)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": l})
}

func (a *API) deleteList(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	if err := a.svc.DeleteList(r.Context(), act, r.PathValue("slug")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// getSaves returns one page of a user's archive as the caller may see it;
// pass the returned next cursor as ?before= for the following page.
func (a *API) getSaves(w http.ResponseWriter, r *http.Request) {
	av, err := a.svc.UserArchive(r.Context(), a.optionalActor(r), r.PathValue("username"), r.URL.Query().Get("before"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saves": av.Entries, "next": av.Next, "note": service.InjectionNote})
}

// putSave saves an item to the caller's archive. A JSON note sets the note
// (an empty string clears it); omitting note keeps the existing one.
func (a *API) putSave(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "item_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Note *string `json:"note"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.svc.SaveItem(r.Context(), act, id, body.Note); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item_id": id})
}

// deleteSave removes an item from the caller's archive and all their tags.
func (a *API) deleteSave(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "item_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.svc.Unsave(r.Context(), act, id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) putEntry(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "item_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Note     string `json:"note"`
		Position *int   `json:"position"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := a.svc.SaveToList(r.Context(), act, r.PathValue("slug"), id, body.Note, body.Position); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "list": r.PathValue("slug"), "item_id": id})
}

func (a *API) deleteEntry(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	id, err := pathID(r, "item_id")
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := a.svc.RemoveFromList(r.Context(), act, r.PathValue("slug"), id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) block(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	if err := a.svc.Block(r.Context(), act, r.PathValue("username"), true); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": true})
}

func (a *API) unblock(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	if err := a.svc.Block(r.Context(), act, r.PathValue("username"), false); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blocked": false})
}

// follow makes the caller follow a user; following twice is fine.
func (a *API) follow(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	if err := a.svc.Follow(r.Context(), act, r.PathValue("username")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": true})
}

// unfollow stops the caller following a user.
func (a *API) unfollow(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	if err := a.svc.Unfollow(r.Context(), act, r.PathValue("username")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"following": false})
}

// followingSaves is one page of recent public saves by people the caller
// follows, newest first.
func (a *API) followingSaves(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	fv, err := a.svc.FollowingFeed(r.Context(), act, r.URL.Query().Get("before"))
	if err != nil {
		writeErr(w, err)
		return
	}
	saves := fv.Saves
	if saves == nil {
		saves = []service.FollowedSave{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"saves": saves, "next": fv.Next, "note": service.InjectionNote})
}

// exchange trades a one-time mobile sign-in code for a device token.
func (a *API) exchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
		DeviceName   string `json:"device_name"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	tok, u, err := a.svc.ExchangeMobileCode(r.Context(), body.Code, body.CodeVerifier, body.DeviceName)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "user": map[string]any{"username": u.Username, "trust_level": u.TrustLevel}})
}

func (a *API) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(strings.ReplaceAll(openapiYAML, "https://dogear.fyi", a.baseURL)))
}

func (a *API) gptSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(strings.ReplaceAll(gptYAML, "https://dogear.fyi", a.baseURL)))
}

// importSaves runs a multipart-uploaded export (field "file") through the
// same import as Settings → import; public_tags=1 makes new tags public.
func (a *API) importSaves(w http.ResponseWriter, r *http.Request, act *service.Actor) {
	r.Body = http.MaxBytesReader(w, r.Body, importfile.MaxBytes+1<<20)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, service.ErrImportTooLarge)
			return
		}
		writeErr(w, &service.Error{Code: "invalid", Message: "send the export as multipart form field \"file\""})
		return
	}
	defer f.Close()
	parsed, err := service.ParseImportFile(hdr.Filename, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.svc.ImportSaves(r.Context(), act, parsed, service.ImportOptions{PublicTags: r.FormValue("public_tags") == "1"})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
