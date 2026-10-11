package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dogear/internal/embed"
)

// fakeEmbeddings embeds text as a hashed bag of words, so texts sharing words
// are close. Enough to exercise the pipeline without the real API.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type d struct {
			Embedding []int `json:"embedding"`
			Index     int   `json:"index"`
		}
		var out []d
		for i, text := range req.Input {
			v := make([]int, embed.Dims)
			for _, w := range strings.Fields(strings.ToLower(text)) {
				h := fnv.New32a()
				_, _ = h.Write([]byte(strings.Trim(w, ".,—:")))
				v[h.Sum32()%embed.Dims] += 40
			}
			for j := range v {
				v[j] = min(v[j], 127)
			}
			out = append(out, d{Embedding: v, Index: i})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
}

func TestHybridSearch(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	srv := fakeEmbeddings(t)
	defer srv.Close()
	s.Embed = &embed.Client{APIKey: "k", URL: srv.URL, HTTP: srv.Client()}
	s.Vectors = embed.NewIndex()

	poster := newUser(t, s, 1)
	seq++
	tag := fmt.Sprintf("zq%d", seq) // unique word so other test rows don't interfere
	target := liveItemTitled(t, s, poster, "Quiet "+tag+" lighthouse keeper journal")
	_ = liveItemTitled(t, s, poster, "Loud stadium "+tag+"x drums")
	for {
		n, err := s.BackfillEmbeddings(ctx, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if !s.SemanticEnabled() {
		t.Fatal("semantic not enabled after backfill")
	}
	// No keyword overlap with the title's unique tag, but shared words.
	page, err := s.Feed(ctx, nil, FeedQuery{Q: "lighthouse keeper " + tag})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || page.Items[0].ID != target {
		t.Fatalf("top result %+v, want %d", page.Items, target)
	}
	sim, err := s.Similar(ctx, nil, target, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range sim {
		if it.ID == target {
			t.Fatal("similar includes the item itself")
		}
	}
}

func liveItemTitled(t *testing.T, s *Service, by *Actor, title string) int64 {
	t.Helper()
	id := liveItem(t, s, by)
	if _, err := s.Pool.Exec(context.Background(), "UPDATE items SET title = $2, blurb = $3 WHERE id = $1", id, title, title); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHybridBadCursor(t *testing.T) {
	s := newTestService(t)
	srv := fakeEmbeddings(t)
	defer srv.Close()
	s.Embed = &embed.Client{APIKey: "k", URL: srv.URL, HTTP: srv.Client()}
	s.Vectors = embed.NewIndex()
	s.Vectors.Put(1, make([]int8, embed.Dims))
	v := make([]int8, embed.Dims)
	v[0] = 1
	s.Vectors.Put(2, v)
	for _, c := range []string{"h|-5", "h|abc", "h|99999999"} {
		cur := base64RawURL(c)
		if _, err := s.hybridFeed(context.Background(), nil, FeedQuery{Q: "x", Cursor: cur}, 30); err == nil {
			t.Errorf("cursor %q accepted", c)
		}
	}
}

func base64RawURL(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
