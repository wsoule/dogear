package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"dogear/internal/auth"
	"dogear/internal/service"
)

const (
	oauthCookie   = "oauth"
	pendingCookie = "pending"
	inviteCookie  = "invite"
	// mobileRedirect is the single allowed redirect target for the native app.
	mobileRedirect = "dogear://auth"
)

type oauthState struct {
	State, Nonce, Provider string
	Mobile                 bool
	Challenge              string // PKCE S256 challenge from the native app
}

type pendingSignup struct {
	Ext       service.ExternalIdentity
	Mobile    bool
	Challenge string
}

type signinPage struct {
	Providers  []string
	InviteOnly bool
	Mobile     bool
}

func (s *Server) providerNames() []string {
	var out []string
	for _, n := range []string{"github", "google", "apple"} {
		if _, ok := s.providers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) signin(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "signin", &Page{Title: "sign in", Nav: "signin", NoIndex: true,
		Data: signinPage{Providers: s.providerNames(), InviteOnly: s.cfg.Signups == "invite", Mobile: r.URL.Query().Get("mobile") == "1"}})
}

// invite stores a valid invite code in a cookie so the signup step can use it.
func (s *Server) invite(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !s.svc.InviteValid(r.Context(), code) {
		s.errorPage(w, r, http.StatusNotFound, "That invite is invalid or has already been used.")
		return
	}
	s.setCookie(w, inviteCookie, code, 30*24*time.Hour, http.SameSiteLaxMode)
	http.Redirect(w, r, "/signin?m=invite", http.StatusSeeOther)
}

// authStart sets a random state (and nonce) cookie and redirects to the provider.
func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("provider")]
	if !ok {
		s.notFound(w, r)
		return
	}
	st := oauthState{State: service.RandomToken(24), Nonce: service.RandomToken(24), Provider: p.Name(),
		Mobile: r.URL.Query().Get("mobile") == "1", Challenge: r.URL.Query().Get("code_challenge")}
	// The native app must send a PKCE challenge; without one the one-time
	// code could be redeemed by any app that registers the dogear:// scheme.
	if st.Mobile && (!service.ValidChallenge(st.Challenge) || r.URL.Query().Get("code_challenge_method") != "S256") {
		s.errorPage(w, r, http.StatusBadRequest, "The app must send code_challenge with code_challenge_method=S256.")
		return
	}
	val, err := s.signer.Sign(oauthCookie, st, 10*time.Minute)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Apple posts the callback cross-site, which drops Lax cookies.
	same := http.SameSiteLaxMode
	if p.Name() == "apple" {
		same = http.SameSiteNoneMode
	}
	s.setCookie(w, oauthCookie, val, 10*time.Minute, same)
	http.Redirect(w, r, p.AuthURL(st.State, st.Nonce), http.StatusFound)
}

func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("provider")
	if r.URL.Path == "/auth/apple/callback" {
		name = "apple"
	}
	p, ok := s.providers[name]
	if !ok {
		s.notFound(w, r)
		return
	}
	if ok, _, retry := s.svc.Limiter.Allow("oauth:"+ClientIP(r), service.OAuthCallbackRules...); !ok {
		w.Header().Set("Retry-After", retryHeader(retry))
		s.errorPage(w, r, http.StatusTooManyRequests, "Too many sign-in attempts. Wait a minute and try again.")
		return
	}
	var st oauthState
	c, err := r.Cookie(oauthCookie)
	if err != nil || s.signer.Verify(oauthCookie, c.Value, &st) != nil || st.Provider != name {
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in expired. Please start again.")
		return
	}
	s.clearCookie(w, oauthCookie)
	state := r.URL.Query().Get("state")
	if r.Method == http.MethodPost {
		state = r.PostFormValue("state")
	}
	if state == "" || state != st.State {
		s.errorPage(w, r, http.StatusBadRequest, "Sign-in state did not match. Please start again.")
		return
	}
	ext, err := p.Callback(ctx, r, st.Nonce)
	if errors.Is(err, auth.ErrDenied) {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	} else if err != nil {
		s.log.Warn("oauth callback", "provider", name, "err", err)
		s.errorPage(w, r, http.StatusBadGateway, "Sign-in with "+name+" failed. Please try again.")
		return
	}

	// Signed in already: link this identity to the current user.
	if v := viewer(r); v != nil {
		if err := s.svc.LinkIdentity(ctx, v.User, ext); err != nil {
			s.serviceError(w, r, err)
			return
		}
		http.Redirect(w, r, "/settings/connections?m=linked", http.StatusSeeOther)
		return
	}
	u, err := s.svc.FindIdentity(ctx, ext.Provider, ext.ProviderUserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if u != nil {
		if err := s.svc.OnLogin(ctx, u, ext); err != nil {
			s.serviceError(w, r, err)
			return
		}
		s.startSession(w, r, u.ID, st.Mobile, st.Challenge, "/")
		return
	}
	// New identity: confirm a username first.
	val, err := s.signer.Sign(pendingCookie, pendingSignup{Ext: ext, Mobile: st.Mobile, Challenge: st.Challenge}, 30*time.Minute)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setCookie(w, pendingCookie, val, 30*time.Minute, http.SameSiteLaxMode)
	http.Redirect(w, r, "/welcome", http.StatusSeeOther)
}

// startSession sets the sid cookie, or for the native app hands back a
// one-time code through the allow-listed custom URL scheme.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64, mobile bool, challenge, next string) {
	if mobile {
		code, err := s.svc.CreateMobileCode(r.Context(), userID, challenge)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, mobileRedirect+"?code="+url.QueryEscape(code), http.StatusFound)
		return
	}
	raw, err := s.svc.CreateSession(r.Context(), userID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setCookie(w, sessionCookie, raw, 30*24*time.Hour, http.SameSiteLaxMode)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

type welcomePage struct {
	Username, Invite, Error, CSRF string
	NeedInvite                    bool
}

func (s *Server) pending(r *http.Request) (pendingSignup, string, bool) {
	var p pendingSignup
	c, err := r.Cookie(pendingCookie)
	if err != nil || s.signer.Verify(pendingCookie, c.Value, &p) != nil {
		return p, "", false
	}
	return p, c.Value, true
}

func (s *Server) welcomeForm(w http.ResponseWriter, r *http.Request) {
	p, raw, ok := s.pending(r)
	if !ok {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	}
	wp := welcomePage{Username: s.svc.SuggestUsername(r.Context(), p.Ext.SuggestedUsername), CSRF: s.signer.CSRFToken(raw),
		NeedInvite: s.cfg.Signups == "invite" && !(p.Ext.Provider == "github" && p.Ext.ProviderUserID == s.cfg.AdminGitHubID)}
	if c, err := r.Cookie(inviteCookie); err == nil {
		wp.Invite = c.Value
	}
	s.render(w, r, http.StatusOK, "welcome", &Page{Title: "welcome", NoIndex: true, Data: wp, TurnstileSiteKey: s.turnstileSignupKey()})
}

func (s *Server) turnstileSignupKey() string {
	if s.turnstile.Enabled() {
		return s.turnstile.SiteKey
	}
	return ""
}

// csrfPending protects the signup form, which has no session yet: its CSRF
// token is derived from the signed pending-signup cookie instead.
func (s *Server) csrfPending(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, raw, ok := s.pending(r)
		if !ok {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		if !s.signer.CheckCSRF(raw, r.FormValue("csrf")) {
			s.errorPage(w, r, http.StatusForbidden, "That form expired. Please sign in again.")
			return
		}
		h(w, r)
	}
}

func (s *Server) welcome(w http.ResponseWriter, r *http.Request) {
	p, raw, _ := s.pending(r)
	wp := welcomePage{Username: r.FormValue("username"), Invite: r.FormValue("invite"), CSRF: s.signer.CSRFToken(raw),
		NeedInvite: s.cfg.Signups == "invite" && !(p.Ext.Provider == "github" && p.Ext.ProviderUserID == s.cfg.AdminGitHubID)}
	fail := func(msg string) {
		wp.Error = msg
		s.render(w, r, http.StatusBadRequest, "welcome", &Page{Title: "welcome", NoIndex: true, Data: wp, TurnstileSiteKey: s.turnstileSignupKey()})
	}
	if !s.turnstile.Verify(r.Context(), r.FormValue("cf-turnstile-response"), ClientIP(r)) {
		fail("Please complete the human check.")
		return
	}
	invite := wp.Invite
	if invite == "" {
		if c, err := r.Cookie(inviteCookie); err == nil {
			invite = c.Value
		}
	}
	u, err := s.svc.SignUp(r.Context(), p.Ext, wp.Username, invite, ClientIP(r))
	if err != nil {
		if e, ok := service.AsError(err); ok {
			fail(e.Message)
			return
		}
		s.serverError(w, r, err)
		return
	}
	s.clearCookie(w, pendingCookie)
	s.clearCookie(w, inviteCookie)
	s.startSession(w, r, u.ID, p.Mobile, p.Challenge, "/?m=welcome")
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if raw := reqCtx(r).sessionRaw; raw != "" {
		_ = s.svc.DeleteSession(r.Context(), raw)
	}
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func retryHeader(d time.Duration) string { return strconv.Itoa(int(d.Seconds()) + 1) }
