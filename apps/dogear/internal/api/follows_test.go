package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/auth"
	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/migrations"
	"dogear/internal/safety"
	"dogear/internal/service"
)

// newTestAPI builds the API on a migrated TEST_DATABASE_URL database and
// skips the test when it is not set.
func newTestAPI(t *testing.T) (*API, *service.Service) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrations.Up(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := &config.Config{Env: "development", BaseURL: "http://test", Signups: "open", IPHashSalt: "x"}
	svc := service.New(pool, cfg, classify.Noop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	noSession := func(*http.Request) (*service.Actor, string) { return nil, "" }
	return New(svc, noSession, auth.Signer{Secret: []byte("test-secret-test-secret-test-sec")}, cfg.BaseURL, safety.ProxyTrust{}), svc
}

// newTokenUser signs up a trusted user and returns them with a bearer token
// holding scopes (nil for every scope).
func newTokenUser(t *testing.T, svc *service.Service, scopes []string) (db.User, string) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("api%d", time.Now().UnixNano()%1_000_000_000)
	u, err := svc.SignUp(ctx, service.ExternalIdentity{Provider: "github", ProviderUserID: name}, name, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetTrust(ctx, u.ID, 1); err != nil {
		t.Fatal(err)
	}
	u.TrustLevel = 1
	raw, _, err := svc.CreateToken(ctx, u, "test", scopes)
	if err != nil {
		t.Fatal(err)
	}
	return u, raw
}

// call sends one API request with a bearer token and decodes the JSON body.
func call(t *testing.T, a *API, method, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestFollowEndpoints(t *testing.T) {
	a, svc := newTestAPI(t)
	ctx := context.Background()
	me, myToken := newTokenUser(t, svc, nil)
	them, theirToken := newTokenUser(t, svc, nil)
	_, readOnly := newTokenUser(t, svc, []string{"read"})

	if code, _ := call(t, a, "PUT", "/users/"+them.Username+"/follow", readOnly); code != http.StatusForbidden {
		t.Fatalf("read-only token follow = %d, want 403", code)
	}
	if code, _ := call(t, a, "PUT", "/users/"+me.Username+"/follow", myToken); code != http.StatusBadRequest {
		t.Fatalf("self follow = %d, want 400", code)
	}
	if code, body := call(t, a, "PUT", "/users/"+them.Username+"/follow", myToken); code != http.StatusOK || body["following"] != true {
		t.Fatalf("follow = %d %v", code, body)
	}

	// Their public save shows in my following feed.
	poster := &service.Actor{User: them, Via: "web"}
	it, err := svc.Q.InsertItem(ctx, db.InsertItemParams{Url: "https://follow-api.test/" + them.Username, UrlNorm: "https://follow-api.test/" + them.Username,
		Domain: "follow-api.test", Title: "Followed thing", Blurb: "A thing.", CreatedBy: &them.ID, Via: "web", Status: "live", Kind: "site", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveItem(ctx, poster, it.ID, nil); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, a, "GET", "/me/following/saves", myToken)
	if code != http.StatusOK {
		t.Fatalf("following saves = %d %v", code, body)
	}
	saves, _ := body["saves"].([]any)
	if len(saves) != 1 {
		t.Fatalf("following saves = %v", body)
	}
	first, _ := saves[0].(map[string]any)
	if first["username"] != them.Username {
		t.Fatalf("save username = %v", first["username"])
	}
	if code, body := call(t, a, "GET", "/me/following/saves", theirToken); code != http.StatusOK || len(body["saves"].([]any)) != 0 {
		t.Fatalf("someone following nobody = %d %v", code, body)
	}
	if code, _ := call(t, a, "GET", "/me/following/saves?before=junk", myToken); code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400", code)
	}

	if code, body := call(t, a, "GET", "/users/"+them.Username, myToken); code != http.StatusOK ||
		body["user"].(map[string]any)["followers"] != float64(1) || body["user"].(map[string]any)["followed_by_me"] != true {
		t.Fatalf("user = %d %v", code, body)
	}
	if code, body := call(t, a, "DELETE", "/users/"+them.Username+"/follow", myToken); code != http.StatusOK || body["following"] != false {
		t.Fatalf("unfollow = %d %v", code, body)
	}
	if _, body := call(t, a, "GET", "/me/following/saves", myToken); len(body["saves"].([]any)) != 0 {
		t.Fatalf("after unfollow = %v", body)
	}
}
