package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"time"

	"dogear/internal/service"
)

const loggitCookie = "loggit_oauth"

type loggitState struct {
	State, Verifier string
	Next            string
}

func (s *Server) loggitRedirectURI() string { return s.cfg.BaseURL + "/integrations/loggit/callback" }

// loggitConnect starts "Sign in with loggit": a PKCE authorization request
// whose state and verifier live in a short-lived signed cookie.
func (s *Server) loggitConnect(w http.ResponseWriter, r *http.Request) {
	if viewer(r) == nil {
		s.needSignIn(w, r)
		return
	}
	if !s.svc.LoggitEnabled() {
		s.notFound(w, r)
		return
	}
	st := loggitState{State: service.RandomToken(24), Verifier: service.RandomToken(48), Next: localPath(r.URL.Query().Get("next"))}
	val, err := s.signer.Sign(loggitCookie, st, 10*time.Minute)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setCookie(w, loggitCookie, val, 10*time.Minute, http.SameSiteLaxMode)
	sum := sha256.Sum256([]byte(st.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	http.Redirect(w, r, s.svc.Loggit.AuthorizeURL(s.loggitRedirectURI(), st.State, challenge), http.StatusFound)
}

func (s *Server) loggitCallback(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	if v == nil {
		s.needSignIn(w, r)
		return
	}
	var st loggitState
	c, err := r.Cookie(loggitCookie)
	if err != nil || s.signer.Verify(loggitCookie, c.Value, &st) != nil || r.URL.Query().Get("state") != st.State {
		s.errorPage(w, r, http.StatusBadRequest, "That loggit connection expired. Start again from settings.")
		return
	}
	s.clearCookie(w, loggitCookie)
	if r.URL.Query().Get("error") != "" {
		http.Redirect(w, r, "/settings/connections", http.StatusSeeOther)
		return
	}
	t, err := s.svc.Loggit.Exchange(r.Context(), r.URL.Query().Get("code"), st.Verifier, s.loggitRedirectURI())
	if err != nil {
		s.log.Warn("loggit exchange", "err", err)
		s.errorPage(w, r, http.StatusBadGateway, "loggit didn't accept the connection. Try again.")
		return
	}
	if err := s.svc.LoggitConnect(r.Context(), v.ID(), t); err != nil {
		s.serverError(w, r, err)
		return
	}
	next := st.Next
	if next == "/" {
		next = "/settings/connections"
	}
	u, _ := url.Parse(next)
	q := u.Query()
	q.Set("m", "loggit")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Server) loggitDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.LoggitDisconnect(r.Context(), viewer(r).ID()); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/connections?m=loggitoff", http.StatusSeeOther)
}

// listLoggit links, unlinks, or syncs a list with loggit.
func (s *Server) listLoggit(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	slug := r.PathValue("slug")
	var err error
	m := "synced"
	switch r.FormValue("op") {
	case "link":
		err = s.svc.LinkList(r.Context(), v, slug, r.FormValue("loggit_list"))
		m = "listlinked"
	case "unlink":
		err = s.svc.UnlinkList(r.Context(), v, slug)
		m = "unlinkedlist"
	default:
		err = s.svc.SyncListNow(r.Context(), v, slug)
	}
	if err != nil {
		if _, ok := service.AsError(err); ok {
			s.serviceError(w, r, err)
			return
		}
		s.log.Warn("loggit list op", "err", err)
	}
	http.Redirect(w, r, "/u/"+url.PathEscape(v.User.Username)+"/"+url.PathEscape(slug)+"?m="+m, http.StatusSeeOther)
}
