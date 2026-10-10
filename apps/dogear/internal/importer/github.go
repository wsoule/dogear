package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when GitHub answers 404 for a repo or README.
var ErrNotFound = errors.New("not found")

const (
	defaultGitHubBaseURL = "https://api.github.com"
	defaultUserAgent     = "DogearBot/1.0"
	maxAttempts          = 6
	starsPause           = time.Second
	maxRateLimitWait     = 15 * time.Minute
	starsBatchSize       = 50
)

// GitHub is a minimal GitHub REST + GraphQL client for the importer.
type GitHub struct {
	Token     string
	HTTP      *http.Client
	BaseURL   string // default https://api.github.com, overridable for tests
	UserAgent string // default "DogearBot/1.0"

	// sleep waits for d or until ctx is done; tests replace it to avoid real waits.
	sleep func(ctx context.Context, d time.Duration) error
}

// Repo is the subset of GET /repos/{owner}/{repo} the importer uses.
type Repo struct {
	FullName      string
	Stars         int
	License       string // SPDX id or "" (NOASSERTION -> "")
	DefaultBranch string
}

// Readme fetches the raw README of repo ("owner/name") and returns the body
// plus the sha256 hex digest of the body, used to detect unchanged lists.
func (g *GitHub) Readme(ctx context.Context, repo string) ([]byte, string, error) {
	body, err := g.do(ctx, http.MethodGet, "/repos/"+repo+"/readme", "application/vnd.github.raw+json", nil)
	if err != nil {
		return nil, "", fmt.Errorf("readme %s: %w", repo, err)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

// Repo fetches repository metadata (stars, SPDX license, default branch).
func (g *GitHub) Repo(ctx context.Context, repo string) (Repo, error) {
	body, err := g.do(ctx, http.MethodGet, "/repos/"+repo, "application/vnd.github+json", nil)
	if err != nil {
		return Repo{}, fmt.Errorf("repo %s: %w", repo, err)
	}
	var raw struct {
		FullName        string `json:"full_name"`
		StargazersCount int    `json:"stargazers_count"`
		DefaultBranch   string `json:"default_branch"`
		License         *struct {
			SPDXID string `json:"spdx_id"`
		} `json:"license"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Repo{}, fmt.Errorf("repo %s: decode: %w", repo, err)
	}
	r := Repo{FullName: raw.FullName, Stars: raw.StargazersCount, DefaultBranch: raw.DefaultBranch}
	if raw.License != nil && raw.License.SPDXID != "NOASSERTION" {
		r.License = raw.License.SPDXID
	}
	return r, nil
}

// Stars fetches stargazer counts through GraphQL, batching up to 100 aliased
// repository(owner:, name:) lookups per query. Repos that GitHub cannot
// resolve are simply absent from the result. Keys are lowercase "owner/repo"
// as requested.
func (g *GitHub) Stars(ctx context.Context, repos []string) (map[string]int, error) {
	seen := map[string]bool{}
	var uniq []string
	for _, r := range repos {
		r = strings.ToLower(strings.TrimSpace(r))
		owner, name, ok := strings.Cut(r, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}

	// Batches run one at a time with a short pause between them: GitHub's
	// secondary rate limits trip quickly on parallel GraphQL queries that
	// each resolve 100 repositories. A failed batch (for example a repo that
	// GitHub blocks) only loses its own counts; the rest are still returned
	// alongside the joined errors.
	out := make(map[string]int, len(uniq))
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 1)
	)
	for start := 0; start < len(uniq); start += starsBatchSize {
		batch := uniq[start:min(start+starsBatchSize, len(uniq))]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			part := map[string]int{}
			err := g.starsBatch(ctx, batch, part)
			_ = g.wait(ctx, starsPause)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			for k, v := range part {
				out[k] = v
			}
		}()
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// starsBatch runs one aliased GraphQL query (r0, r1, ...) and stores the
// stargazer counts of the repos that resolved into out.
func (g *GitHub) starsBatch(ctx context.Context, batch []string, out map[string]int) error {
	var q strings.Builder
	q.WriteString("query {")
	for i, r := range batch {
		owner, name, _ := strings.Cut(r, "/")
		o, _ := json.Marshal(owner)
		n, _ := json.Marshal(name)
		fmt.Fprintf(&q, " r%d: repository(owner: %s, name: %s) { stargazerCount }", i, o, n)
	}
	q.WriteString(" }")
	payload, err := json.Marshal(map[string]string{"query": q.String()})
	if err != nil {
		return err
	}
	body, err := g.do(ctx, http.MethodPost, "/graphql", "application/json", payload)
	if err != nil {
		return fmt.Errorf("graphql stars: %w", err)
	}
	var resp struct {
		Data   map[string]*struct{ StargazerCount int } `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("graphql stars: decode: %w", err)
	}
	for i, r := range batch {
		if v := resp.Data["r"+strconv.Itoa(i)]; v != nil {
			out[r] = v.StargazerCount
		}
	}
	if len(resp.Data) == 0 {
		for _, e := range resp.Errors {
			if e.Type != "NOT_FOUND" {
				return fmt.Errorf("graphql stars: %s: %s", e.Type, e.Message)
			}
		}
	}
	return nil
}

// do performs one API request with auth and rate-limit handling: on a 403 or
// 429 that carries Retry-After, or X-RateLimit-Remaining "0" with
// X-RateLimit-Reset, it waits (at most 15 minutes, honoring ctx) and retries,
// up to 6 attempts. A 404 maps to ErrNotFound; other non-2xx are errors.
func (g *GitHub) do(ctx context.Context, method, path, accept string, payload []byte) ([]byte, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = defaultGitHubBaseURL
	}
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ua := g.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	for attempt := 1; ; attempt++ {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, reqBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return body, nil
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
			if wait, ok := rateLimitWait(resp.Header, time.Now()); ok && attempt < maxAttempts {
				if err := g.wait(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
		}
		return nil, fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, snippet(body))
	}
}

// rateLimitWait derives how long to back off from GitHub rate-limit headers.
// ok is false when the response is not a rate-limit response.
func rateLimitWait(h http.Header, now time.Time) (time.Duration, bool) {
	var d time.Duration
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		secs, err := strconv.Atoi(ra)
		if err != nil {
			return 0, false
		}
		d = time.Duration(secs) * time.Second
	} else if h.Get("X-RateLimit-Remaining") == "0" {
		reset, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64)
		if err != nil {
			return 0, false
		}
		d = time.Unix(reset, 0).Sub(now) + time.Second
	} else {
		return 0, false
	}
	return max(0, min(d, maxRateLimitWait)), true
}

// wait sleeps for d unless ctx is cancelled first.
func (g *GitHub) wait(ctx context.Context, d time.Duration) error {
	if g.sleep != nil {
		return g.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// snippet shortens an error response body for inclusion in error messages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
