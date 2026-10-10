// Package loggit is a small client for loggit's OAuth provider and lists
// API, used to keep Dogear lists and loggit lists in two-way sync. The API
// contract lives in .agents/ignore/specs/2026-10-09-dogear-lists.md.
package loggit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("loggit: not found")
	ErrUnauthorized = errors.New("loggit: unauthorized")
)

// DuplicateError is returned when adding media that is already on a list.
type DuplicateError struct{ EntryID string }

func (e *DuplicateError) Error() string { return "loggit: duplicate entry " + e.EntryID }

type Client struct {
	APIURL, WebURL         string
	ClientID, ClientSecret string
	HTTP                   *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Enabled reports whether the loggit integration is configured.
func (c *Client) Enabled() bool {
	return c != nil && c.APIURL != "" && c.WebURL != "" && c.ClientID != "" && c.ClientSecret != ""
}

// Scopes requested from loggit.
const Scopes = "profile lists:read lists:write"

// AuthorizeURL is where the browser goes to approve Dogear on loggit.
func (c *Client) AuthorizeURL(redirectURI, state, codeChallenge string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {redirectURI},
		"scope": {Scopes}, "state": {state}, "code_challenge": {codeChallenge}, "code_challenge_method": {"S256"}}
	return strings.TrimRight(c.WebURL, "/") + "/oauth/authorize?" + q.Encode()
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	User         User   `json:"user"`
}

// Exchange trades an authorization code (with its PKCE verifier) for tokens.
func (c *Client) Exchange(ctx context.Context, code, verifier, redirectURI string) (Token, error) {
	return c.token(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "redirect_uri": {redirectURI}})
}

// Refresh exchanges a refresh token for a new pair (loggit rotates them).
func (c *Client) Refresh(ctx context.Context, refresh string) (Token, error) {
	return c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}

func (c *Client) token(ctx context.Context, form url.Values) (Token, error) {
	form.Set("client_id", c.ClientID)
	form.Set("client_secret", c.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/oauth/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var t Token
	if err := c.do(req, &t); err != nil {
		return Token{}, err
	}
	if t.AccessToken == "" {
		return Token{}, fmt.Errorf("loggit: token response without access_token")
	}
	return t, nil
}

// Revoke invalidates a token; failures are ignored by callers.
func (c *Client) Revoke(ctx context.Context, token string) error {
	body, _ := json.Marshal(map[string]string{"token": token, "client_id": c.ClientID, "client_secret": c.ClientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/oauth/revoke"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

type MediaRef struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	ReleaseYear *int   `json:"releaseYear"`
	CoverImage  string `json:"coverImage"`
	URL         string `json:"url"`
	LoggitURL   string `json:"loggitUrl"`
}

type Entry struct {
	ID        string    `json:"id"`
	ListID    string    `json:"listId"`
	MediaID   string    `json:"mediaId"`
	Note      string    `json:"note"`
	Position  int       `json:"position"`
	UpdatedAt time.Time `json:"updatedAt"`
	DeletedAt *string   `json:"deletedAt"`
	Media     MediaRef  `json:"media"`
}

type List struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Visibility  string    `json:"visibility"`
	Ranked      bool      `json:"ranked"`
	EntryCount  int       `json:"entryCount"`
	UpdatedAt   time.Time `json:"updatedAt"`
	DeletedAt   *string   `json:"deletedAt"`
	URL         string    `json:"url"`
	Entries     []Entry   `json:"entries"`
}

type ListInput struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Visibility  *string `json:"visibility,omitempty"`
}

// Lists returns the user's lists.
func (c *Client) Lists(ctx context.Context, access string) ([]List, error) {
	var out struct {
		Lists []List `json:"lists"`
	}
	if err := c.call(ctx, access, http.MethodGet, "/lists", nil, &out); err != nil {
		return nil, err
	}
	return out.Lists, nil
}

// GetList returns one list with its live entries ordered by position.
func (c *Client) GetList(ctx context.Context, access, id string) (List, error) {
	var l List
	err := c.call(ctx, access, http.MethodGet, "/lists/"+url.PathEscape(id), nil, &l)
	return l, err
}

func (c *Client) CreateList(ctx context.Context, access string, in ListInput) (List, error) {
	var l List
	err := c.call(ctx, access, http.MethodPost, "/lists", in, &l)
	return l, err
}

func (c *Client) UpdateList(ctx context.Context, access, id string, in ListInput) (List, error) {
	var l List
	err := c.call(ctx, access, http.MethodPatch, "/lists/"+url.PathEscape(id), in, &l)
	return l, err
}

func (c *Client) AddEntry(ctx context.Context, access, listID, mediaID, note string) (Entry, error) {
	var e Entry
	err := c.call(ctx, access, http.MethodPost, "/lists/"+url.PathEscape(listID)+"/entries",
		map[string]any{"mediaId": mediaID, "note": note}, &e)
	return e, err
}

func (c *Client) UpdateEntryNote(ctx context.Context, access, listID, entryID, note string) error {
	return c.call(ctx, access, http.MethodPatch, "/lists/"+url.PathEscape(listID)+"/entries/"+url.PathEscape(entryID),
		map[string]any{"note": note}, nil)
}

func (c *Client) DeleteEntry(ctx context.Context, access, listID, entryID string) error {
	err := c.call(ctx, access, http.MethodDelete, "/lists/"+url.PathEscape(listID)+"/entries/"+url.PathEscape(entryID), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) SetOrder(ctx context.Context, access, listID string, entryIDs []string) error {
	return c.call(ctx, access, http.MethodPut, "/lists/"+url.PathEscape(listID)+"/order", map[string]any{"entryIds": entryIDs}, nil)
}

// Resolve maps a URL to a loggit catalog item; ok is false when loggit
// cannot represent it.
func (c *Client) Resolve(ctx context.Context, access, rawURL string) (MediaRef, bool, error) {
	var out struct {
		Media MediaRef `json:"media"`
	}
	err := c.call(ctx, access, http.MethodPost, "/media/resolve", map[string]string{"url": rawURL}, &out)
	if errors.Is(err, ErrNotFound) {
		return MediaRef{}, false, nil
	}
	if err != nil {
		return MediaRef{}, false, err
	}
	return out.Media, out.Media.ID != "", nil
}

func (c *Client) api(path string) string { return strings.TrimRight(c.APIURL, "/") + path }

func (c *Client) call(ctx context.Context, access, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api(path), rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+access)
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dogear/1.0 (+loggit sync)")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode == http.StatusConflict:
		var d struct {
			EntryID string `json:"entryId"`
		}
		_ = json.Unmarshal(b, &d)
		return &DuplicateError{EntryID: d.EntryID}
	case resp.StatusCode == http.StatusBadRequest && strings.Contains(string(b), "invalid_grant"):
		return ErrUnauthorized
	case resp.StatusCode >= 300:
		msg := string(b)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("loggit %s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, msg)
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("loggit decode %s: %w", req.URL.Path, err)
		}
	}
	return nil
}
