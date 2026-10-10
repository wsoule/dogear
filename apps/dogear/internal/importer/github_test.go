package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestGitHub returns a client pointed at an httptest server whose sleeps
// are recorded instead of performed.
func newTestGitHub(t *testing.T, h http.HandlerFunc) (*GitHub, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var waits []time.Duration
	g := &GitHub{Token: "tok", HTTP: srv.Client(), BaseURL: srv.URL, UserAgent: "DogearBot/1.0"}
	g.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	return g, &waits
}

func TestReadme(t *testing.T) {
	body := "# Awesome\n\n- [x](https://x.dev)\n"
	g, _ := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/missing/repo/readme" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/repos/sindresorhus/awesome/readme" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github.raw+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "DogearBot/1.0" {
			t.Errorf("User-Agent = %q", got)
		}
		io.WriteString(w, body)
	})
	got, sha, err := g.Readme(context.Background(), "sindresorhus/awesome")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if string(got) != body || sha != hex.EncodeToString(sum[:]) {
		t.Errorf("Readme = %q, %q", got, sha)
	}
	if _, _, err := g.Readme(context.Background(), "missing/repo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing readme err = %v, want ErrNotFound", err)
	}
}

func TestNoTokenNoAuthHeader(t *testing.T) {
	g, _ := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("Authorization = %q, want none", h)
		}
		io.WriteString(w, "x")
	})
	g.Token = ""
	if _, _, err := g.Readme(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
}

func TestRepo(t *testing.T) {
	g, _ := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/avelino/awesome-go":
			io.WriteString(w, `{"full_name":"avelino/awesome-go","stargazers_count":140000,"default_branch":"main","license":{"key":"mit","spdx_id":"MIT"}}`)
		case "/repos/x/other":
			io.WriteString(w, `{"full_name":"x/other","stargazers_count":3,"default_branch":"master","license":{"key":"other","spdx_id":"NOASSERTION"}}`)
		case "/repos/x/none":
			io.WriteString(w, `{"full_name":"x/none","stargazers_count":0,"default_branch":"main","license":null}`)
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	cases := map[string]Repo{
		"avelino/awesome-go": {FullName: "avelino/awesome-go", Stars: 140000, License: "MIT", DefaultBranch: "main"},
		"x/other":            {FullName: "x/other", Stars: 3, DefaultBranch: "master"},
		"x/none":             {FullName: "x/none", DefaultBranch: "main"},
	}
	for name, want := range cases {
		got, err := g.Repo(ctx, name)
		if err != nil || got != want {
			t.Errorf("Repo(%s) = %+v, %v; want %+v", name, got, err, want)
		}
	}
	if _, err := g.Repo(ctx, "gone/repo"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestStarsBatching(t *testing.T) {
	aliasRe := regexp.MustCompile(`(r\d+): repository\(owner: "([^"]+)", name: "([^"]+)"\)`)
	var mu sync.Mutex
	queries := 0
	g, _ := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		queries++
		mu.Unlock()
		matches := aliasRe.FindAllStringSubmatch(req.Query, -1)
		if len(matches) > 100 {
			t.Errorf("query has %d aliases, want <= 100", len(matches))
		}
		data := map[string]any{}
		var errs []map[string]any
		for _, m := range matches {
			if m[3] == "repo-7" { // simulate a deleted repo
				data[m[1]] = nil
				errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": "Could not resolve"})
				continue
			}
			var n int
			fmt.Sscanf(m[3], "repo-%d", &n)
			data[m[1]] = map[string]any{"stargazerCount": n * 10}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
	})
	var repos []string
	for i := range 150 {
		repos = append(repos, fmt.Sprintf("Owner/Repo-%d", i))
	}
	repos = append(repos, "owner/repo-1", "not-a-repo")
	got, err := g.Stars(context.Background(), repos)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 2 {
		t.Errorf("queries = %d, want 2", queries)
	}
	if len(got) != 149 {
		t.Errorf("len = %d, want 149", len(got))
	}
	if got["owner/repo-42"] != 420 {
		t.Errorf("owner/repo-42 = %d", got["owner/repo-42"])
	}
	if _, ok := got["owner/repo-7"]; ok {
		t.Error("missing repo should be absent")
	}
}

func TestRetryAfter429(t *testing.T) {
	calls := 0
	g, waits := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, "ok")
	})
	body, _, err := g.Readme(context.Background(), "a/b")
	if err != nil || string(body) != "ok" {
		t.Fatalf("Readme = %q, %v", body, err)
	}
	if calls != 2 || len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Errorf("calls=%d waits=%v", calls, *waits)
	}
}

func TestRateLimitResetGivesUpAfterThreeAttempts(t *testing.T) {
	calls := 0
	g, waits := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := g.Repo(context.Background(), "a/b")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403 error", err)
	}
	if calls != 3 || len(*waits) != 2 {
		t.Errorf("calls=%d waits=%v", calls, *waits)
	}
	for _, d := range *waits {
		if d != maxRateLimitWait {
			t.Errorf("wait %v, want capped at %v", d, maxRateLimitWait)
		}
	}
}

func TestPlainForbiddenIsNotRetried(t *testing.T) {
	calls := 0
	g, _ := newTestGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := g.Repo(context.Background(), "a/b"); err == nil || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestWaitHonorsContext(t *testing.T) {
	g := &GitHub{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
