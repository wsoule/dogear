package auth

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"dogear/internal/service"
)

// GitHub signs in with a GitHub OAuth app using the default (public) scope.
type GitHub struct {
	Config *oauth2.Config
	HTTP   *http.Client
	APIURL string
}

func NewGitHub(clientID, secret, baseURL string) *GitHub {
	return &GitHub{
		Config: &oauth2.Config{ClientID: clientID, ClientSecret: secret, Endpoint: github.Endpoint,
			RedirectURL: baseURL + "/auth/github/callback"},
		HTTP:   &http.Client{Timeout: 10 * time.Second},
		APIURL: "https://api.github.com",
	}
}

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) AuthURL(state, _ string) string {
	return g.Config.AuthCodeURL(state, oauth2.SetAuthURLParam("allow_signup", "true"))
}

func (g *GitHub) Callback(ctx context.Context, r *http.Request, _ string) (service.ExternalIdentity, error) {
	code := r.URL.Query().Get("code")
	if code == "" {
		return service.ExternalIdentity{}, ErrDenied
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.HTTP)
	tok, err := g.Config.Exchange(ctx, code)
	if err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("github exchange: %w", err)
	}
	var u struct {
		ID        int64     `json:"id"`
		Login     string    `json:"login"`
		Name      string    `json:"name"`
		AvatarURL string    `json:"avatar_url"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := getJSON(ctx, g.HTTP, g.APIURL+"/user", tok.AccessToken, &u); err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("github user: %w", err)
	}
	created := u.CreatedAt
	return service.ExternalIdentity{Provider: "github", ProviderUserID: strconv.FormatInt(u.ID, 10),
		SuggestedUsername: u.Login, DisplayName: u.Name, AvatarURL: u.AvatarURL, ProviderCreatedAt: &created}, nil
}

// googleEndpoint is declared inline because x/oauth2/google pulls in GCP
// metadata dependencies this app does not need.
var googleEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// Google signs in with OpenID Connect, requesting only "openid profile".
type Google struct {
	Config *oauth2.Config
	HTTP   *http.Client
}

func NewGoogle(clientID, secret, baseURL string) *Google {
	return &Google{
		Config: &oauth2.Config{ClientID: clientID, ClientSecret: secret, Endpoint: googleEndpoint,
			RedirectURL: baseURL + "/auth/google/callback", Scopes: []string{"openid", "profile"}},
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *Google) Name() string { return "google" }

func (g *Google) AuthURL(state, nonce string) string {
	return g.Config.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce), oauth2.SetAuthURLParam("prompt", "select_account"))
}

func (g *Google) Callback(ctx context.Context, r *http.Request, _ string) (service.ExternalIdentity, error) {
	code := r.URL.Query().Get("code")
	if code == "" {
		return service.ExternalIdentity{}, ErrDenied
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, g.HTTP)
	tok, err := g.Config.Exchange(ctx, code)
	if err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("google exchange: %w", err)
	}
	// The userinfo endpoint is called over TLS with the fresh access token,
	// so the response is authentic without verifying the ID token locally.
	var u struct {
		Sub     string `json:"sub"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := getJSON(ctx, g.HTTP, "https://openidconnect.googleapis.com/v1/userinfo", tok.AccessToken, &u); err != nil {
		return service.ExternalIdentity{}, fmt.Errorf("google userinfo: %w", err)
	}
	if u.Sub == "" {
		return service.ExternalIdentity{}, fmt.Errorf("google userinfo: missing sub")
	}
	return service.ExternalIdentity{Provider: "google", ProviderUserID: u.Sub, SuggestedUsername: u.Name,
		DisplayName: u.Name, AvatarURL: u.Picture}, nil
}
