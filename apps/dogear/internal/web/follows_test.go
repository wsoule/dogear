package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/migrations"
	"dogear/internal/service"
)

// newTestServer builds the web server on a migrated TEST_DATABASE_URL
// database and skips the test when it is not set.
func newTestServer(t *testing.T) (*Server, *service.Service) {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrations.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := &config.Config{Env: "development", BaseURL: "http://test", Signups: "open", IPHashSalt: "x",
		SessionSecret: []byte("test-secret-test-secret-test-sec")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(pool, cfg, classify.Noop{}, log)
	svc.AutoTagSync = true
	srv, err := New(Options{Service: svc, Config: cfg, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return srv, svc
}

// signedIn signs up a user and returns them with a session cookie value.
func signedIn(t *testing.T, svc *service.Service) (db.User, string) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("web%d", time.Now().UnixNano()%1_000_000_000)
	u, err := svc.SignUp(ctx, service.ExternalIdentity{Provider: "github", ProviderUserID: name}, name, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := svc.CreateSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u, sid
}

// do sends one request through the full middleware chain as a signed-in
// user and returns the response.
func do(t *testing.T, srv *Server, method, path, sid string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestFollowButton(t *testing.T) {
	srv, svc := newTestServer(t)
	me, sid := signedIn(t, svc)
	them, _ := signedIn(t, svc)
	csrf := srv.signer.CSRFToken(sid)
	path := "/u/" + them.Username + "/follow"

	// The profile offers a follow button to other signed-in viewers.
	rec := do(t, srv, "GET", "/u/"+them.Username, sid, nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `action="`+path+`"`) {
		t.Fatalf("profile = %d, follow form missing", rec.Code)
	}
	// Without the CSRF token the follow is refused.
	if rec := do(t, srv, "POST", path, sid, url.Values{"follow": {"1"}}, true); rec.Code != http.StatusForbidden {
		t.Fatalf("follow without csrf = %d, want 403", rec.Code)
	}
	// htmx gets the refreshed box: one follower, button now "following".
	rec = do(t, srv, "POST", path, sid, url.Values{"follow": {"1"}, "csrf": {csrf}}, true)
	b := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(b, `id="follow-box"`) || !strings.Contains(b, "<b>1</b> <span>follower</span>") ||
		!strings.Contains(b, `aria-pressed="true"`) || strings.Contains(b, "<html") {
		t.Fatalf("htmx follow = %d %s", rec.Code, b)
	}
	if !svc.IsFollowing(context.Background(), &service.Actor{User: me}, them.ID) {
		t.Fatal("follow did not stick")
	}
	// A plain form unfollows and redirects back to the profile.
	rec = do(t, srv, "POST", path, sid, url.Values{"follow": {"0"}, "csrf": {csrf}}, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/u/"+them.Username {
		t.Fatalf("form unfollow = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if svc.IsFollowing(context.Background(), &service.Actor{User: me}, them.ID) {
		t.Fatal("unfollow did not stick")
	}
}

func TestFollowingPage(t *testing.T) {
	srv, svc := newTestServer(t)
	_, sid := signedIn(t, svc)
	if rec := do(t, srv, "GET", "/following", "", nil, false); rec.Code != http.StatusSeeOther {
		t.Fatalf("signed-out /following = %d, want redirect to sign in", rec.Code)
	}
	rec := do(t, srv, "GET", "/following", sid, nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nothing here yet") {
		t.Fatalf("empty /following = %d", rec.Code)
	}

	// Follow a trusted user who saved something: /following lists it with
	// a "saved by" line, and the home strip shows it to someone else.
	ctx := context.Background()
	me, _ := svc.SessionUser(ctx, sid)
	them, _ := signedIn(t, svc)
	if err := svc.SetTrust(ctx, them.ID, 1); err != nil {
		t.Fatal(err)
	}
	them.TrustLevel = 1
	u := "https://follow-web.test/" + them.Username
	it, err := svc.Q.InsertItem(ctx, db.InsertItemParams{Url: u, UrlNorm: u, Domain: "follow-web.test", Title: "Followed web thing",
		Blurb: "A thing.", CreatedBy: &them.ID, Via: "web", Status: "live", Kind: "site", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveItem(ctx, &service.Actor{User: them, Via: "web"}, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.Follow(ctx, &service.Actor{User: *me, Via: "web"}, them.Username); err != nil {
		t.Fatal(err)
	}
	rec = do(t, srv, "GET", "/following", sid, nil, false)
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, "Followed web thing") || !strings.Contains(b, `class="follow-by`) {
		t.Fatalf("/following = %d, missing the followed save", rec.Code)
	}
	rec = do(t, srv, "GET", "/", sid, nil, false)
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, "recently saved") || !strings.Contains(b, "Followed web thing") {
		t.Fatalf("home = %d, missing the recently saved strip", rec.Code)
	}
}
