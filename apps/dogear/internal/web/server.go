// Package web serves the server-rendered HTML UI. Handlers only parse input
// and render output; every rule lives in the service package.
package web

import (
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"dogear/internal/auth"
	"dogear/internal/config"
	"dogear/internal/ghsync"
	"dogear/internal/safety"
	"dogear/internal/service"
)

type Server struct {
	svc       *service.Service
	cfg       *config.Config
	log       *slog.Logger
	tmpl      templates
	signer    auth.Signer
	turnstile auth.Turnstile
	providers map[string]auth.Provider
	app       *ghsync.App
	mux       *http.ServeMux
	ogCache   *pngCache // rendered share preview images
}

type Options struct {
	Service   *service.Service
	Config    *config.Config
	Log       *slog.Logger
	Providers map[string]auth.Provider
	App       *ghsync.App // nil when the GitHub App is not configured
	// API and MCP are mounted at /api/v1/ and /mcp.
	API, MCP http.Handler
}

func New(o Options) (*Server, error) {
	t, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{
		svc: o.Service, cfg: o.Config, log: o.Log, tmpl: t,
		signer:    auth.Signer{Secret: o.Config.SessionSecret},
		turnstile: auth.Turnstile{SiteKey: o.Config.TurnstileSiteKey, Secret: o.Config.TurnstileSecretKey},
		providers: o.Providers, app: o.App, mux: http.NewServeMux(),
		ogCache: newPNGCache(256),
	}
	s.routes(o.API, o.MCP)
	return s, nil
}

// Signer exposes the cookie/CSRF signer so the API can check CSRF headers.
func (s *Server) Signer() auth.Signer { return s.signer }

func (s *Server) routes(api, mcp http.Handler) {
	m := s.mux
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", cacheForever(http.StripPrefix("/static/", http.FileServerFS(static))))
	m.HandleFunc("GET /sw.js", s.serviceWorker)
	m.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/favicon.svg", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /robots.txt", s.robots)
	m.HandleFunc("GET /sitemap.xml", s.sitemap)
	m.HandleFunc("GET /llms.txt", s.llms)
	m.HandleFunc("GET /manifest.webmanifest", s.manifest)
	m.HandleFunc("GET /offline", s.staticPage("offline", "offline"))

	m.HandleFunc("GET /{$}", s.home)
	m.HandleFunc("GET /new", s.newFeed)
	m.HandleFunc("GET /c/{slug}", s.category)
	m.HandleFunc("GET /k/{kind}", s.kind)
	m.HandleFunc("GET /t/{slug}", s.topic)
	m.HandleFunc("GET /categories", s.categories)
	m.HandleFunc("GET /lists", s.lists)
	m.HandleFunc("GET /view/{mode}", s.setView)
	m.HandleFunc("GET /search", s.search)
	m.HandleFunc("GET /i/{id}", s.item)
	m.HandleFunc("GET /img/{id}", s.thumb)
	m.HandleFunc("GET /about", s.staticPage("about", "about"))
	m.HandleFunc("GET /terms", s.staticPage("terms", "terms"))
	m.HandleFunc("GET /privacy", s.staticPage("privacy", "privacy"))

	m.HandleFunc("PUT /items/{id}/vote", s.csrf(s.vote))
	m.HandleFunc("POST /items/{id}/vote", s.csrf(s.vote))
	m.HandleFunc("DELETE /items/{id}/vote", s.csrf(s.vote))
	m.HandleFunc("GET /items/{id}/save", s.saveMenu)
	m.HandleFunc("POST /items/{id}/save", s.csrf(s.save))
	m.HandleFunc("POST /items/{id}/dogear", s.csrf(s.dogear))
	m.HandleFunc("POST /items/{id}/report", s.csrf(s.report))

	m.HandleFunc("GET /submit", s.submitForm)
	m.HandleFunc("POST /submit", s.csrf(s.submit))
	m.HandleFunc("GET /submit/preview", s.preview)

	m.HandleFunc("GET /u/{username}", s.profile)
	m.HandleFunc("POST /u/{username}/block", s.csrf(s.block))
	m.HandleFunc("POST /u/{username}/follow", s.csrf(s.follow))
	m.HandleFunc("GET /following", s.following)
	m.HandleFunc("GET /u/{username}/{slug}", s.list)
	// Share surfaces. Tag slugs are [a-z0-9-] only, so "feed.xml" never
	// names a tag and the literal routes below can't shadow a tag page.
	m.HandleFunc("GET /u/{username}/feed.xml", s.rssArchive)
	m.HandleFunc("GET /u/{username}/{slug}/feed.xml", s.rssTag)
	m.HandleFunc("GET /u/{username}/{slug}/embed", s.embed)
	m.HandleFunc("GET /og/u/{file}", s.ogArchive)
	m.HandleFunc("GET /og/u/{username}/{file}", s.ogTag)
	m.HandleFunc("GET /share", s.share)
	m.HandleFunc("POST /lists", s.csrf(s.createList))
	m.HandleFunc("POST /lists/{slug}", s.csrf(s.editList))
	m.HandleFunc("POST /lists/{slug}/entries/{id}", s.csrf(s.editEntry))

	m.HandleFunc("GET /signin", s.signin)
	m.HandleFunc("GET /invite/{code}", s.invite)
	m.HandleFunc("GET /auth/{provider}", s.authStart)
	m.HandleFunc("GET /auth/{provider}/callback", s.authCallback)
	m.HandleFunc("POST /auth/apple/callback", s.authCallback)
	m.HandleFunc("GET /welcome", s.welcomeForm)
	m.HandleFunc("POST /welcome", s.csrfPending(s.welcome))
	m.HandleFunc("POST /auth/logout", s.csrf(s.logout))

	m.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/profile", http.StatusSeeOther)
	})
	m.HandleFunc("GET /settings/{tab}", s.settings)
	m.HandleFunc("POST /settings/profile", s.csrf(s.saveProfile))
	m.HandleFunc("POST /settings/tokens", s.csrf(s.createToken))
	m.HandleFunc("POST /settings/tokens/{id}/revoke", s.csrf(s.revokeToken))
	m.HandleFunc("POST /settings/import", s.limitImportBody(s.csrf(s.importSaves)))
	m.HandleFunc("POST /settings/connections/{provider}/unlink", s.csrf(s.unlink))
	m.HandleFunc("POST /settings/connections/repo", s.csrf(s.repoSettings))
	m.HandleFunc("POST /settings/invites", s.csrf(s.createInvite))
	m.HandleFunc("GET /settings/account/export", s.export)
	m.HandleFunc("POST /settings/account/delete", s.csrf(s.deleteAccount))

	m.HandleFunc("GET /integrations/loggit/connect", s.loggitConnect)
	m.HandleFunc("GET /integrations/loggit/callback", s.loggitCallback)
	m.HandleFunc("POST /integrations/loggit/disconnect", s.csrf(s.loggitDisconnect))
	m.HandleFunc("POST /lists/{slug}/loggit", s.csrf(s.listLoggit))
	m.HandleFunc("GET /github/setup", s.githubSetup)
	m.HandleFunc("POST /webhooks/github", s.githubWebhook)

	m.HandleFunc("GET /mod", s.mod)
	m.HandleFunc("POST /mod/item/{id}", s.csrf(s.modItem))
	m.HandleFunc("POST /mod/report/{id}", s.csrf(s.modReport))
	m.HandleFunc("POST /mod/user", s.csrf(s.modUser))
	m.HandleFunc("POST /mod/domain", s.csrf(s.modDomain))

	if registerTestLogin != nil && s.cfg.Staging() {
		registerTestLogin(s)
	}
	if api != nil {
		m.Handle("/api/v1/", api)
	}
	if mcp != nil {
		m.Handle("/mcp", mcp)
	}
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) })
}

// registerTestLogin is set only in builds with the "staging" tag.
var registerTestLogin func(s *Server)

// Handler returns the full middleware chain.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.logRequests(s.secure(gzipMiddleware(s.loadSession(s.mux)))))
}

type ctxKey struct{}

type requestContext struct {
	actor      *service.Actor
	sessionRaw string
	csrf       string
}

func reqCtx(r *http.Request) *requestContext {
	if rc, ok := r.Context().Value(ctxKey{}).(*requestContext); ok {
		return rc
	}
	return &requestContext{}
}

func viewer(r *http.Request) *service.Actor { return reqCtx(r).actor }

const sessionCookie = "sid"

// loadSession resolves the sid cookie to a signed-in actor for every request.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := &requestContext{}
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			u, err := s.svc.SessionUser(r.Context(), c.Value)
			if err != nil {
				s.log.Error("session", "err", err)
			}
			if u != nil {
				rc.actor = &service.Actor{User: *u, Via: "web"}
				rc.sessionRaw = c.Value
				rc.csrf = s.signer.CSRFToken(c.Value)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, rc)))
	})
}

// SessionActor lets the API authenticate cookie requests: it returns the
// signed-in actor and whether the CSRF header matches.
func SessionActor(r *http.Request) (*service.Actor, string) {
	rc := reqCtx(r)
	return rc.actor, rc.sessionRaw
}

// csrf rejects state-changing requests without a valid per-session token,
// sent either as the X-CSRF-Token header (htmx) or a "csrf" form field.
func (s *Server) csrf(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := reqCtx(r)
		if rc.actor == nil {
			s.needSignIn(w, r)
			return
		}
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = r.FormValue("csrf")
		}
		if !s.signer.CheckCSRF(rc.sessionRaw, tok) {
			s.errorPage(w, r, http.StatusForbidden, "That form expired. Go back, reload, and try again.")
			return
		}
		h(w, r)
	}
}

func (s *Server) needSignIn(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", "/signin")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

// secure sets the security headers required on every response.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		script, frame := "'self'", "'none'"
		if s.turnstile.Enabled() && (r.URL.Path == "/welcome" || r.URL.Path == "/submit") {
			script += " https://challenges.cloudflare.com"
			frame = "https://challenges.cloudflare.com"
		}
		h.Set("Content-Security-Policy", "default-src 'self'; script-src "+script+"; style-src 'self'; img-src 'self' data:; frame-src "+frame+"; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://github.com https://accounts.google.com https://appleid.apple.com")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.Production() {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		// xff_hops (never the addresses) shows how many proxies sit in front,
		// which decides which X-Forwarded-For entry ClientIP should trust.
		hops := 0
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			hops = strings.Count(xff, ",") + 1
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "xff_hops", hops, "via_cf", r.Header.Get("CF-Connecting-IP") != "" && s.clientIP(r) == strings.TrimSpace(r.Header.Get("CF-Connecting-IP")))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "err", v, "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// WriteHeader drops any Content-Length set upstream (http.FileServer sets
// one), since the compressed body has a different length.
func (g gzipWriter) WriteHeader(code int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

func (g gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
func (g gzipWriter) Flush() {
	_ = g.gz.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// gzipMiddleware compresses HTML, CSS, JS, and JSON responses so pages stay
// well under the 50 KB budget. The MCP stream is left alone.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.URL.Path == "/mcp" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzPool.Put(gz)
		}()
		next.ServeHTTP(gzipWriter{ResponseWriter: w, gz: gz}, r)
	})
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fingerprinted URLs (?v=<hash>) never change content, so they can be
		// cached for a year; bare URLs get a short lifetime.
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		h.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's address for rate limiting and signup hashing.
func (s *Server) clientIP(r *http.Request) string { return safety.ClientIPWith(r, s.cfg.ProxyTrust()) }

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.BaseURL, "https://") }

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: s.secureCookies() || sameSite == http.SameSiteNoneMode, SameSite: sameSite})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies()})
}
