// Package service is the single entry point for business rules. The web UI,
// REST API, and MCP server all call it, so validation, rate limits, and trust
// rules live in one place. It never touches net/http request handling.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/safety"
)

type Service struct {
	Pool       *pgxpool.Pool
	Q          *db.Queries
	Cfg        *config.Config
	Limiter    *safety.Limiter
	Classifier classify.Classifier
	Thresholds classify.Thresholds
	// Fetch is the SSRF-safe client used for page previews and classification.
	Fetch *http.Client
	// API is a plain client for trusted third-party APIs (Safe Browsing, oEmbed).
	API *http.Client
	Log *slog.Logger
	// UninstallApp removes a GitHub App installation when an account is
	// deleted. Nil when the App is not configured.
	UninstallApp func(ctx context.Context, installationID int64) error

	classifyCache *ttlCache[classify.Result]
	catCache      *ttlCache[[]*CategoryNode]
	feedCache     *ttlCache[FeedPage]
}

func New(pool *pgxpool.Pool, cfg *config.Config, cl classify.Classifier, log *slog.Logger) *Service {
	ua := "DogearBot/1.0 (+" + cfg.BaseURL + "/about)"
	return &Service{
		Pool:          pool,
		Q:             db.New(pool),
		Cfg:           cfg,
		Limiter:       safety.NewLimiter(),
		Classifier:    cl,
		Thresholds:    classify.DefaultThresholds(),
		Fetch:         safety.NewClient(safety.ClientOptions{Timeout: 10 * time.Second, MaxRedirects: 5, UserAgent: ua}),
		API:           &http.Client{Timeout: 5 * time.Second},
		Log:           log,
		classifyCache: newTTLCache[classify.Result](7 * 24 * time.Hour),
		catCache:      newTTLCache[[]*CategoryNode](time.Minute),
		feedCache:     newTTLCache[FeedPage](time.Minute),
	}
}

// Error is a user-facing failure with a stable code that the API maps to an
// HTTP status. Codes: invalid, unauthorized, forbidden, not_found, duplicate,
// rate_limited, rejected.
type Error struct {
	Code       string
	Message    string
	ItemID     int64
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

var (
	ErrUnauthorized = &Error{Code: "unauthorized", Message: "sign in required"}
	ErrNotFound     = &Error{Code: "not_found", Message: "not found"}
)

// AsError extracts a service Error from err.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Actor is who is acting: a signed-in web user or an API token's owner.
type Actor struct {
	User    db.User
	TokenID *int64
	Scopes  []string // nil for session-authenticated web requests (all scopes)
	Via     string   // web, api, mcp
}

// Can reports whether the actor holds a token scope. Sessions hold all scopes.
func (a *Actor) Can(scope string) bool {
	if a == nil {
		return false
	}
	return a.Scopes == nil || slices.Contains(a.Scopes, scope)
}

func (a *Actor) ID() int64 { return a.User.ID }

func (a *Actor) IsMod() bool { return a != nil && (a.User.Role == "mod" || a.User.Role == "admin") }

// require checks the actor is signed in, not banned, and holds scope.
func require(a *Actor, scope string) error {
	if a == nil {
		return ErrUnauthorized
	}
	if a.User.BannedAt != nil {
		return errf("forbidden", "this account is banned")
	}
	if scope != "" && !a.Can(scope) {
		return errf("forbidden", "token lacks the %q scope", scope)
	}
	return nil
}

// inTx runs fn inside a transaction, committing on success.
func (s *Service) inTx(ctx context.Context, fn func(q *db.Queries, tx pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(s.Q.WithTx(tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func notFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// limit applies rate-limit rules for key, returning a rate_limited Error.
func (s *Service) limit(key string, rules ...safety.Rule) error {
	ok, _, retry := s.Limiter.Allow(key, rules...)
	if ok {
		return nil
	}
	return &Error{Code: "rate_limited", Message: "slow down and try again later", RetryAfter: retry}
}

// Remaining reports how many actions remain in the tightest bucket for a key.
func (s *Service) Remaining(key string, rules ...safety.Rule) int {
	return s.Limiter.Peek(key, rules...)
}
