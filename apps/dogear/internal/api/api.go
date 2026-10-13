// Package api serves the JSON REST API at /api/v1. Handlers only parse
// input and render output; rules live in the service package.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dogear/internal/auth"
	"dogear/internal/safety"
	"dogear/internal/service"
)

//go:embed openapi.yaml
var openapiYAML string

//go:embed gpt.yaml
var gptYAML string

// SessionFunc returns the cookie-authenticated actor and raw session value.
type SessionFunc func(*http.Request) (*service.Actor, string)

type API struct {
	svc     *service.Service
	session SessionFunc
	signer  auth.Signer
	baseURL string
	// proxy describes the proxies in front of the app; see safety.ClientIPWith.
	proxy safety.ProxyTrust
	mux   *http.ServeMux
	// Routes lists every registered pattern, used by the spec-coverage test.
	Routes []string
}

var (
	readRules      = []safety.Rule{{Limit: 300, Per: time.Minute}}
	tokenReadRules = []safety.Rule{{Limit: 600, Per: time.Minute}}
)

func New(svc *service.Service, session SessionFunc, signer auth.Signer, baseURL string, proxy safety.ProxyTrust) *API {
	a := &API{svc: svc, session: session, signer: signer, baseURL: baseURL, proxy: proxy, mux: http.NewServeMux()}
	a.handle("GET /categories", a.categories)
	a.handle("GET /topics", a.topics)
	a.handle("GET /items", a.listItems)
	a.handle("GET /items/{id}", a.getItem)
	a.handle("POST /items", a.auth("post", a.createItem))
	a.handle("PUT /items/{id}/vote", a.auth("vote", a.vote))
	a.handle("DELETE /items/{id}/vote", a.auth("vote", a.unvote))
	a.handle("POST /items/{id}/report", a.auth("vote", a.report))
	a.handle("GET /users/{username}", a.getUser)
	a.handle("GET /users/{username}/lists/{slug}", a.getList)
	a.handle("GET /users/{username}/saves", a.getSaves)
	a.handle("PUT /users/{username}/follow", a.auth("vote", a.follow))
	a.handle("DELETE /users/{username}/follow", a.auth("vote", a.unfollow))
	a.handle("GET /me", a.auth("read", a.me))
	a.handle("PATCH /me", a.sessionOnly(a.patchMe))
	a.handle("DELETE /me", a.sessionOnly(a.deleteMe))
	a.handle("GET /me/items", a.auth("read", a.myItems))
	a.handle("GET /me/export", a.auth("read", a.export))
	a.handle("GET /me/following/saves", a.auth("read", a.followingSaves))
	a.handle("GET /me/lists", a.auth("read", a.myLists))
	a.handle("POST /me/lists", a.auth("post", a.createList))
	a.handle("PATCH /me/lists/{slug}", a.auth("post", a.patchList))
	a.handle("DELETE /me/lists/{slug}", a.auth("post", a.deleteList))
	a.handle("PUT /me/lists/{slug}/items/{item_id}", a.auth("post", a.putEntry))
	a.handle("DELETE /me/lists/{slug}/items/{item_id}", a.auth("post", a.deleteEntry))
	a.handle("PUT /me/saves/{item_id}", a.auth("post", a.putSave))
	a.handle("DELETE /me/saves/{item_id}", a.auth("post", a.deleteSave))
	a.handle("PUT /blocks/{username}", a.auth("vote", a.block))
	a.handle("DELETE /blocks/{username}", a.auth("vote", a.unblock))
	a.handle("POST /auth/exchange", a.exchange)
	a.handle("GET /openapi.yaml", a.openapi)
	a.handle("GET /gpt.yaml", a.gptSpec)
	return a
}

func (a *API) handle(pattern string, h http.HandlerFunc) {
	a.Routes = append(a.Routes, pattern)
	method, path, _ := strings.Cut(pattern, " ")
	a.mux.HandleFunc(method+" /api/v1"+path, h)
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Every request counts against a read bucket: per token when a valid
	// bearer token is sent, otherwise per client IP. An invalid or junk
	// Authorization header falls back to the IP bucket, so it cannot be used
	// to skip the limit.
	key, rules := "read:ip:"+safety.ClientIPWith(r, a.proxy), readRules
	if raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if act, err := a.svc.TokenActor(r.Context(), strings.TrimSpace(raw), "api"); err == nil && act.TokenID != nil {
			key, rules = "read:tok:"+strconv.FormatInt(*act.TokenID, 10), tokenReadRules
		}
	}
	ok, rem, retry := a.svc.Limiter.Allow(key, rules...)
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(rem))
	if !ok {
		writeErr(w, &service.Error{Code: "rate_limited", Message: "too many requests", RetryAfter: retry})
		return
	}
	if _, pattern := a.mux.Handler(r); pattern == "" {
		writeErr(w, &service.Error{Code: "not_found", Message: "no such endpoint"})
		return
	}
	a.mux.ServeHTTP(w, r)
}

type ctxActor struct{}

// actor authenticates a request: a bearer token, or a session cookie plus a
// valid X-CSRF-Token header for state-changing methods. Bearer requests are
// CSRF-exempt because they carry no ambient credentials.
func (a *API) actor(r *http.Request) (*service.Actor, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			return nil, service.ErrUnauthorized
		}
		via := "api"
		if c := r.Header.Get("X-Dogear-Client") + r.UserAgent(); c == "mcp" || strings.Contains(c, "dogear-claude") {
			via = "mcp"
		}
		return a.svc.TokenActor(r.Context(), strings.TrimSpace(raw), via)
	}
	act, raw := a.session(r)
	if act == nil {
		return nil, nil
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.signer.CheckCSRF(raw, r.Header.Get("X-CSRF-Token")) {
		return nil, &service.Error{Code: "forbidden", Message: "missing or invalid X-CSRF-Token header"}
	}
	return act, nil
}

// optionalActor is used by public reads to attach my_vote when signed in.
func (a *API) optionalActor(r *http.Request) *service.Actor {
	act, err := a.actor(r)
	if err != nil {
		return nil
	}
	return act
}

type authedHandler func(w http.ResponseWriter, r *http.Request, act *service.Actor)

// auth requires a signed-in actor holding scope.
func (a *API) auth(scope string, h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act, err := a.actor(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if act == nil {
			writeErr(w, service.ErrUnauthorized)
			return
		}
		if !act.Can(scope) {
			writeErr(w, &service.Error{Code: "forbidden", Message: "token lacks the " + scope + " scope"})
			return
		}
		if act.TokenID != nil {
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(a.svc.PostRemaining(act)))
		}
		h(w, r, act)
	}
}

// sessionOnly refuses bearer tokens: account deletion and profile edits
// require the website session plus the CSRF header.
func (a *API) sessionOnly(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			writeErr(w, &service.Error{Code: "forbidden", Message: "this endpoint requires a signed-in browser session; tokens cannot call it"})
			return
		}
		act, err := a.actor(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if act == nil {
			writeErr(w, service.ErrUnauthorized)
			return
		}
		h(w, r, act)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("X-RateLimit-Remaining") == "" {
		w.Header().Set("X-RateLimit-Remaining", "-1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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

// writeErr renders {"error": {"code", "message", "item_id"}}; unknown errors
// become a generic 500 without leaking details.
func writeErr(w http.ResponseWriter, err error) {
	// A canceled request means the client hung up; answer 499 (nginx's
	// "client closed request") so logs don't count it as a server error.
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(499)
		return
	}
	var e *service.Error
	if !errors.As(err, &e) {
		e = &service.Error{Code: "internal", Message: "internal error"}
	}
	body := map[string]any{"code": e.Code, "message": e.Message}
	if e.ItemID != 0 {
		body["item_id"] = e.ItemID
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Seconds())+1))
		w.Header().Set("X-RateLimit-Remaining", "0")
	}
	writeJSON(w, statusFor(e.Code), map[string]any{"error": body})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		return &service.Error{Code: "invalid", Message: "body must be valid JSON"}
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, &service.Error{Code: "invalid", Message: name + " must be a number"}
	}
	return id, nil
}
