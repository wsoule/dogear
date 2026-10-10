package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"dogear/internal/db"
	"dogear/internal/ghsync"
	"dogear/internal/service"
)

type providerRow struct {
	Name   string
	Linked bool
}

type settingsPage struct {
	Tab           string
	Error         string
	NewToken      string
	Tokens        []db.ApiToken
	Identities    []db.Identity
	Providers     []providerRow
	Sync          *db.GithubSync
	AppConfigured bool
	TemplateURL   string
	InstallURL    string
	Invites       []db.ListInvitesRow
	NewInvite     string
	InviteLimit   int
	LoggitEnabled bool
	Loggit        *db.LoggitAccount
}

func (s *Server) settingsData(r *http.Request, tab string) (settingsPage, error) {
	v := viewer(r)
	ctx := r.Context()
	sp := settingsPage{Tab: tab, AppConfigured: s.app != nil, InviteLimit: 3}
	var err error
	switch tab {
	case "tokens":
		sp.Tokens, err = s.svc.Tokens(ctx, v.ID())
	case "connections":
		if sp.Identities, err = s.svc.Identities(ctx, v.ID()); err != nil {
			return sp, err
		}
		for _, n := range s.providerNames() {
			row := providerRow{Name: n}
			for _, id := range sp.Identities {
				if id.Provider == n {
					row.Linked = true
				}
			}
			sp.Providers = append(sp.Providers, row)
		}
		if s.app != nil {
			sp.TemplateURL = ghsync.TemplateURL(v.User.Username)
			sp.InstallURL = ghsync.InstallURL(s.app.Slug)
		}
		sp.Sync, err = s.svc.SyncStatus(ctx, v.ID())
		sp.LoggitEnabled = s.svc.LoggitEnabled()
		sp.Loggit = s.svc.LoggitAccount(ctx, v.ID())
	case "invites":
		sp.Invites, err = s.svc.Invites(ctx, v.ID())
	case "account":
		sp.Sync, err = s.svc.SyncStatus(ctx, v.ID())
	}
	return sp, err
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if viewer(r) == nil {
		s.needSignIn(w, r)
		return
	}
	tab := r.PathValue("tab")
	switch tab {
	case "profile", "tokens", "connections", "invites", "account":
	default:
		s.notFound(w, r)
		return
	}
	sp, err := s.settingsData(r, tab)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "settings", &Page{Title: "settings", Nav: "me", NoIndex: true, Data: sp})
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, tab string, err error) {
	e, ok := service.AsError(err)
	if !ok {
		s.serverError(w, r, err)
		return
	}
	sp, derr := s.settingsData(r, tab)
	if derr != nil {
		s.serverError(w, r, derr)
		return
	}
	sp.Error = e.Message
	s.render(w, r, statusFor(e.Code), "settings", &Page{Title: "settings", Nav: "me", NoIndex: true, Data: sp})
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	_, err := s.svc.UpdateProfile(r.Context(), v.User, service.ProfileInput{DisplayName: r.FormValue("display_name"),
		Bio: r.FormValue("bio"), Website: r.FormValue("website")})
	if err != nil {
		s.settingsError(w, r, "profile", err)
		return
	}
	http.Redirect(w, r, "/settings/profile?m=profile", http.StatusSeeOther)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	_ = r.ParseForm()
	raw, _, err := s.svc.CreateToken(r.Context(), v.User, r.FormValue("name"), r.Form["scope"])
	if err != nil {
		s.settingsError(w, r, "tokens", err)
		return
	}
	sp, err := s.settingsData(r, "tokens")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sp.NewToken = raw
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "settings", &Page{Title: "settings", Nav: "me", NoIndex: true, Data: sp})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.svc.RevokeToken(r.Context(), viewer(r).ID(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/tokens?m=revoked", http.StatusSeeOther)
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.UnlinkIdentity(r.Context(), viewer(r).User, r.PathValue("provider")); err != nil {
		s.settingsError(w, r, "connections", err)
		return
	}
	http.Redirect(w, r, "/settings/connections?m=unlinked", http.StatusSeeOther)
}

func (s *Server) repoSettings(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	var err error
	m := "saved"
	if r.FormValue("op") == "disconnect" {
		err = s.svc.DisconnectRepo(r.Context(), v.ID())
		m = "disconnect"
	} else {
		err = s.svc.SetIncludePosted(r.Context(), v.ID(), r.FormValue("include_posted") == "1")
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/connections?m="+m, http.StatusSeeOther)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	code, err := s.svc.CreateInvite(r.Context(), viewer(r).User)
	if err != nil {
		s.settingsError(w, r, "invites", err)
		return
	}
	sp, err := s.settingsData(r, "invites")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sp.NewInvite = code
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, http.StatusOK, "settings", &Page{Title: "settings", Nav: "me", NoIndex: true, Data: sp})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	if v == nil {
		s.needSignIn(w, r)
		return
	}
	data, err := s.svc.Export(r.Context(), v)
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="dogear-`+v.User.Username+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	err := s.svc.DeleteAccount(r.Context(), viewer(r), r.FormValue("confirm"), r.FormValue("delete_posts") == "1")
	if err != nil {
		s.settingsError(w, r, "account", err)
		return
	}
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, r, "/?m=deleted", http.StatusSeeOther)
}

// githubSetup is where GitHub sends the user after installing the App.
func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request) {
	v := viewer(r)
	if v == nil {
		s.needSignIn(w, r)
		return
	}
	if s.app == nil {
		s.notFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Missing installation id from GitHub.")
		return
	}
	if _, err := s.svc.ConnectRepo(r.Context(), v.User, s.app, id); err != nil {
		s.settingsError(w, r, "connections", err)
		return
	}
	http.Redirect(w, r, "/settings/connections?m=connected", http.StatusSeeOther)
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubAppWebhookSecret == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !ghsync.VerifySignature([]byte(s.cfg.GitHubAppWebhookSecret), body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	ev, err := ghsync.ParseWebhook(r.Header.Get("X-GitHub-Event"), body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if err := s.svc.HandleWebhook(r.Context(), ev); err != nil {
		s.log.Error("webhook", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type modPage struct{ Q service.ModQueue }

func (s *Server) mod(w http.ResponseWriter, r *http.Request) {
	q, err := s.svc.ModQueue(r.Context(), viewer(r))
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "mod", &Page{Title: "mod", Nav: "mod", NoIndex: true, Data: modPage{Q: q}})
}

func (s *Server) modItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.svc.ModItem(r.Context(), viewer(r), id, r.FormValue("action"), r.FormValue("note")); err != nil {
		s.serviceError(w, r, err)
		return
	}
	s.back(w, r, "/mod")
}

func (s *Server) modReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.svc.ModReport(r.Context(), viewer(r), id, r.FormValue("action") == "uphold"); err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/mod", http.StatusSeeOther)
}

func (s *Server) modUser(w http.ResponseWriter, r *http.Request) {
	err := s.svc.ModUser(r.Context(), viewer(r), r.FormValue("username"), r.FormValue("action"), r.FormValue("cascade") == "1")
	if err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/mod?m=done", http.StatusSeeOther)
}

func (s *Server) modDomain(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.BlockDomain(r.Context(), viewer(r), r.FormValue("domain"), "blocked by mod"); err != nil {
		s.serviceError(w, r, err)
		return
	}
	http.Redirect(w, r, "/mod?m=done", http.StatusSeeOther)
}
