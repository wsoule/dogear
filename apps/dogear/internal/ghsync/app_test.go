package ghsync

import (
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func pkcs1PEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestJWTVerifies(t *testing.T) {
	k := rsaKey(t)
	now := time.Unix(1_700_000_000, 0)
	for name, keyPEM := range map[string][]byte{"pkcs1": pkcs1PEM(k), "pkcs8": pkcs8PEM(t, k)} {
		t.Run(name, func(t *testing.T) {
			a := &App{AppID: 42, PrivateKeyPEM: keyPEM}
			tok, err := a.JWT(now)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(tok, ".")
			if len(parts) != 3 {
				t.Fatalf("jwt parts = %d", len(parts))
			}
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
				t.Fatalf("signature: %v", err)
			}
			hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
			if !strings.Contains(string(hdr), `"alg":"RS256"`) {
				t.Errorf("header %s", hdr)
			}
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Iat, Exp int64
				Iss      string
			}
			if err := json.Unmarshal(raw, &claims); err != nil {
				t.Fatal(err)
			}
			if claims.Iat != now.Unix()-60 || claims.Exp != now.Unix()+540 || claims.Iss != "42" {
				t.Errorf("claims %+v", claims)
			}
		})
	}
	if _, err := (&App{PrivateKeyPEM: []byte("nope")}).JWT(now); err == nil {
		t.Error("expected error for bad key")
	}
}

// fakeGitHub is a minimal GitHub API for app tests.
type fakeGitHub struct {
	t           *testing.T
	mu          sync.Mutex
	tokenCalls  atomic.Int32
	readmeSHA   string // "" means README does not exist
	repoGone    bool
	conflicts   int // number of PUTs to reject with 409 before accepting
	puts        []map[string]string
	putStatuses []int
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "bad jwt", 401)
			return
		}
		n := f.tokenCalls.Add(1)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_" + r.PathValue("id") + "_" + string(rune('0'+n)),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "account": map[string]any{"login": "ada", "id": 99}})
	})
	mux.HandleFunc("DELETE /app/installations/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []any{map[string]any{"full_name": "ada/awesome-ada"}}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/README.md", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.repoGone || f.readmeSHA == "" {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sha": f.readmeSHA})
	})
	mux.HandleFunc("PUT /repos/{owner}/{repo}/contents/README.md", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]string
		data, _ := io.ReadAll(r.Body)
		json.Unmarshal(data, &body)
		f.puts = append(f.puts, body)
		status := 200
		switch {
		case f.repoGone:
			status = 404
		case f.conflicts > 0:
			f.conflicts--
			f.readmeSHA = "newer"
			status = 409
		case body["sha"] != f.readmeSHA:
			status = 422
		default:
			if f.readmeSHA == "" {
				status = 201
			}
			f.readmeSHA = "sha" + string(rune('0'+len(f.puts)))
		}
		f.putStatuses = append(f.putStatuses, status)
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
	})
	return mux
}

func newTestApp(t *testing.T, f *fakeGitHub) *App {
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return &App{AppID: 1, PrivateKeyPEM: pkcs1PEM(rsaKey(t)), BaseURL: srv.URL, HTTP: srv.Client(), Slug: "dogear-sync"}
}

func TestInstallationTokenCached(t *testing.T) {
	f := &fakeGitHub{t: t}
	a := newTestApp(t, f)
	ctx := context.Background()
	var wg sync.WaitGroup
	tok1, err := a.InstallationToken(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := a.InstallationToken(ctx, 7)
			if err != nil || tok != tok1 {
				t.Errorf("tok=%q err=%v", tok, err)
			}
		}()
	}
	wg.Wait()
	if n := f.tokenCalls.Load(); n != 1 {
		t.Errorf("token fetches = %d, want 1", n)
	}
	if _, err := a.InstallationToken(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if n := f.tokenCalls.Load(); n != 2 {
		t.Errorf("token fetches = %d, want 2", n)
	}
	// Expire the cache entry and expect a refetch.
	a.mu.Lock()
	c := a.tokens[7]
	c.expiresAt = time.Now().Add(-time.Second)
	a.tokens[7] = c
	a.mu.Unlock()
	tok2, _ := a.InstallationToken(ctx, 7)
	if tok2 == tok1 || f.tokenCalls.Load() != 3 {
		t.Errorf("expected refetch, tok2=%q calls=%d", tok2, f.tokenCalls.Load())
	}
}

func TestInstallationHelpers(t *testing.T) {
	f := &fakeGitHub{t: t}
	a := newTestApp(t, f)
	ctx := context.Background()
	repos, err := a.InstallationRepos(ctx, 7)
	if err != nil || len(repos) != 1 || repos[0] != "ada/awesome-ada" {
		t.Errorf("repos=%v err=%v", repos, err)
	}
	login, id, err := a.InstallationAccount(ctx, 7)
	if err != nil || login != "ada" || id != 99 {
		t.Errorf("login=%q id=%d err=%v", login, id, err)
	}
	if err := a.DeleteInstallation(ctx, 7); err != nil {
		t.Error(err)
	}
}

func TestPutReadme(t *testing.T) {
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		f := &fakeGitHub{t: t}
		a := newTestApp(t, f)
		if err := a.PutReadme(ctx, 7, "ada/awesome-ada", []byte("hello"), "Update list (1 item)"); err != nil {
			t.Fatal(err)
		}
		if len(f.puts) != 1 || f.puts[0]["sha"] != "" || f.putStatuses[0] != 201 {
			t.Fatalf("puts=%v statuses=%v", f.puts, f.putStatuses)
		}
		if got, _ := base64.StdEncoding.DecodeString(f.puts[0]["content"]); string(got) != "hello" {
			t.Errorf("content %q", got)
		}
		if f.puts[0]["message"] != "Update list (1 item)" {
			t.Errorf("message %q", f.puts[0]["message"])
		}
	})

	t.Run("update", func(t *testing.T) {
		f := &fakeGitHub{t: t, readmeSHA: "abc"}
		a := newTestApp(t, f)
		if err := a.PutReadme(ctx, 7, "ada/awesome-ada", []byte("x"), "m"); err != nil {
			t.Fatal(err)
		}
		if len(f.puts) != 1 || f.puts[0]["sha"] != "abc" || f.putStatuses[0] != 200 {
			t.Fatalf("puts=%v statuses=%v", f.puts, f.putStatuses)
		}
	})

	t.Run("conflict retry", func(t *testing.T) {
		f := &fakeGitHub{t: t, readmeSHA: "abc", conflicts: 1}
		a := newTestApp(t, f)
		if err := a.PutReadme(ctx, 7, "ada/awesome-ada", []byte("x"), "m"); err != nil {
			t.Fatal(err)
		}
		if len(f.puts) != 2 || f.puts[1]["sha"] != "newer" {
			t.Fatalf("puts=%v statuses=%v", f.puts, f.putStatuses)
		}
	})

	t.Run("conflict twice fails", func(t *testing.T) {
		f := &fakeGitHub{t: t, readmeSHA: "abc", conflicts: 2}
		a := newTestApp(t, f)
		err := a.PutReadme(ctx, 7, "ada/awesome-ada", []byte("x"), "m")
		if err == nil || errors.Is(err, ErrRepoGone) || len(f.puts) != 2 {
			t.Fatalf("err=%v puts=%d", err, len(f.puts))
		}
	})

	t.Run("gone", func(t *testing.T) {
		f := &fakeGitHub{t: t, repoGone: true}
		a := newTestApp(t, f)
		err := a.PutReadme(ctx, 7, "ada/awesome-ada", []byte("x"), "m")
		if !errors.Is(err, ErrRepoGone) {
			t.Fatalf("err=%v, want ErrRepoGone", err)
		}
	})

	t.Run("bad name", func(t *testing.T) {
		a := newTestApp(t, &fakeGitHub{t: t})
		if err := a.PutReadme(ctx, 7, "nope", nil, "m"); err == nil {
			t.Error("expected error")
		}
	})
}

func TestURLs(t *testing.T) {
	if got, want := InstallURL("dogear-sync"), "https://github.com/apps/dogear-sync/installations/new"; got != want {
		t.Errorf("InstallURL = %q", got)
	}
	got := TemplateURL("ada")
	for _, part := range []string{"https://github.com/new?", "template_owner=dogear", "template_name=dogear-list-template", "name=awesome-ada"} {
		if !strings.Contains(got, part) {
			t.Errorf("TemplateURL %q missing %q", got, part)
		}
	}
}
