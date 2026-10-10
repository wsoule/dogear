//go:build staging

package web

import (
	"encoding/json"
	"net/http"
	"time"

	"dogear/internal/service"
)

// Staging builds expose /test/login?user=<name> so the red-team script can
// create sessions without OAuth. Production builds omit this file entirely
// (no "staging" build tag), and it also requires ENV=staging at runtime.
func init() {
	registerTestLogin = func(s *Server) {
		s.mux.HandleFunc("GET /test/login", func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("user")
			ctx := r.Context()
			ext := service.ExternalIdentity{Provider: "github", ProviderUserID: "test-" + name, SuggestedUsername: name}
			u, err := s.svc.FindIdentity(ctx, ext.Provider, ext.ProviderUserID)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			if u == nil {
				signups := s.cfg.Signups
				s.cfg.Signups = "open"
				nu, err := s.svc.SignUp(ctx, ext, name, "", s.clientIP(r))
				s.cfg.Signups = signups
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				u = &nu
			}
			raw, err := s.svc.CreateSession(ctx, u.ID)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			s.setCookie(w, sessionCookie, raw, 24*time.Hour, http.SameSiteLaxMode)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"username": u.Username, "csrf": s.signer.CSRFToken(raw)})
		})
	}
}
