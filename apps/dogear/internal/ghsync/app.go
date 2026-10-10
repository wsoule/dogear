package ghsync

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrRepoGone means the synced repo (or the App's access to it) no longer
// exists; the caller should pause syncing for that user.
var ErrRepoGone = errors.New("ghsync: repository not accessible")

const (
	defaultBaseURL   = "https://api.github.com"
	userAgent        = "DogearBot/1.0"
	apiVersion       = "2022-11-28"
	tokenMaxCacheAge = 50 * time.Minute
	templateOwner    = "dogear"
	templateRepo     = "dogear-list-template"
)

// App authenticates as the Dogear GitHub App and calls the GitHub REST API.
type App struct {
	AppID         int64
	PrivateKeyPEM []byte
	BaseURL       string // default https://api.github.com
	HTTP          *http.Client
	Slug          string

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

// cachedToken is an installation access token held in memory until expiresAt.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// apiError is a non-success GitHub response, kept so callers can branch on status.
type apiError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("github %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// statusOf returns the HTTP status carried by an apiError, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// TemplateURL is GitHub's "use this template" page for the list template,
// prefilled with the suggested repo name awesome-<username>.
func TemplateURL(username string) string {
	q := url.Values{}
	q.Set("template_owner", templateOwner)
	q.Set("template_name", templateRepo)
	q.Set("name", "awesome-"+username)
	q.Set("visibility", "public")
	return "https://github.com/new?" + q.Encode()
}

// InstallURL is the page where a user installs the App on their repos.
func InstallURL(appSlug string) string {
	return "https://github.com/apps/" + url.PathEscape(appSlug) + "/installations/new"
}

// JWT builds a short-lived RS256 app JWT (iat now-60s to tolerate clock
// drift, exp now+9m, under GitHub's 10 minute limit, iss AppID).
func (a *App) JWT(now time.Time) (string, error) {
	key, err := parseRSAKey(a.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{now.Add(-60 * time.Second).Unix(), now.Add(9 * time.Minute).Unix(), strconv.FormatInt(a.AppID, 10)})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("ghsync: sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// parseRSAKey reads an RSA private key from PEM in PKCS#1 ("RSA PRIVATE KEY",
// what GitHub hands out) or PKCS#8 ("PRIVATE KEY") form.
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("ghsync: private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ghsync: parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("ghsync: private key is not RSA")
	}
	return rk, nil
}

// InstallationToken returns an installation access token, reusing an
// in-memory copy for up to 50 minutes (or until a minute before GitHub's
// expiry) and otherwise minting one with the app JWT. Safe for concurrent use.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	now := time.Now()
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && now.Before(t.expiresAt) {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()

	jwt, err := a.JWT(now)
	if err != nil {
		return "", err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := a.doJSON(ctx, http.MethodPost, path, jwt, nil, &resp); err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", errors.New("ghsync: empty installation token")
	}
	expires := now.Add(tokenMaxCacheAge)
	if !resp.ExpiresAt.IsZero() && resp.ExpiresAt.Add(-time.Minute).Before(expires) {
		expires = resp.ExpiresAt.Add(-time.Minute)
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[int64]cachedToken{}
	}
	a.tokens[installationID] = cachedToken{token: resp.Token, expiresAt: expires}
	a.mu.Unlock()
	return resp.Token, nil
}

// forgetToken drops a cached token, e.g. after GitHub rejects it with 401.
func (a *App) forgetToken(installationID int64) {
	a.mu.Lock()
	delete(a.tokens, installationID)
	a.mu.Unlock()
}

// InstallationRepos lists the full names of every repo the installation can
// access, following pagination.
func (a *App) InstallationRepos(ctx context.Context, installationID int64) ([]string, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var names []string
	for page := 1; ; page++ {
		var resp struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := a.doJSON(ctx, http.MethodGet, path, tok, nil, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Repositories {
			names = append(names, r.FullName)
		}
		if len(resp.Repositories) < 100 || len(names) >= resp.TotalCount {
			return names, nil
		}
	}
}

// InstallationAccount returns the login and numeric id of the account the
// installation belongs to, so the caller can match it to the user's identity.
func (a *App) InstallationAccount(ctx context.Context, installationID int64) (string, int64, error) {
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", 0, err
	}
	var resp struct {
		Account struct {
			Login string `json:"login"`
			ID    int64  `json:"id"`
		} `json:"account"`
	}
	path := fmt.Sprintf("/app/installations/%d", installationID)
	if err := a.doJSON(ctx, http.MethodGet, path, jwt, nil, &resp); err != nil {
		return "", 0, err
	}
	return resp.Account.Login, resp.Account.ID, nil
}

// DeleteInstallation uninstalls the App from the account (used on account
// deletion). An installation that is already gone counts as success.
func (a *App) DeleteInstallation(ctx context.Context, installationID int64) error {
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return err
	}
	a.forgetToken(installationID)
	path := fmt.Sprintf("/app/installations/%d", installationID)
	err = a.doJSON(ctx, http.MethodDelete, path, jwt, nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// PutReadme writes README.md through the Contents API. It reads the current
// blob sha (absent means create), then PUTs the new content; on a sha
// conflict (409/422) it refetches the sha and retries once. A 403/404 from
// GitHub returns ErrRepoGone.
func (a *App) PutReadme(ctx context.Context, installationID int64, repoFullName string, content []byte, message string) error {
	owner, repo, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return fmt.Errorf("ghsync: invalid repo name %q", repoFullName)
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/README.md"

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := a.InstallationToken(ctx, installationID)
		if err != nil {
			if s := statusOf(err); s == http.StatusNotFound || s == http.StatusForbidden {
				return fmt.Errorf("%w: %v", ErrRepoGone, err)
			}
			return err
		}
		sha, err := a.readmeSHA(ctx, tok, path)
		if err != nil {
			if statusOf(err) == http.StatusUnauthorized {
				a.forgetToken(installationID)
			}
			if statusOf(err) == http.StatusForbidden {
				return fmt.Errorf("%w: %v", ErrRepoGone, err)
			}
			return err
		}
		body := map[string]string{
			"message": message,
			"content": base64.StdEncoding.EncodeToString(content),
		}
		if sha != "" {
			body["sha"] = sha
		}
		err = a.doJSON(ctx, http.MethodPut, path, tok, body, nil)
		if err == nil {
			return nil
		}
		switch statusOf(err) {
		case http.StatusConflict, http.StatusUnprocessableEntity:
			lastErr = err
			continue
		case http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("%w: %v", ErrRepoGone, err)
		case http.StatusUnauthorized:
			a.forgetToken(installationID)
			return err
		default:
			return err
		}
	}
	return lastErr
}

// readmeSHA returns the blob sha of README.md, or "" when it does not exist yet.
func (a *App) readmeSHA(ctx context.Context, token, path string) (string, error) {
	var resp struct {
		SHA string `json:"sha"`
	}
	err := a.doJSON(ctx, http.MethodGet, path, token, nil, &resp)
	if statusOf(err) == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return resp.SHA, nil
}

// doJSON sends an authenticated GitHub API request with an optional JSON body
// and decodes a JSON response into out (when non-nil). Non-2xx responses
// return an *apiError.
func (a *App) doJSON(ctx context.Context, method, path, bearer string, in, out any) error {
	base := a.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{Method: method, Path: path, Status: res.StatusCode, Body: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("ghsync: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}
