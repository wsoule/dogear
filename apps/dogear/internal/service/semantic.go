package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/embed"
)

// SemanticEnabled reports whether semantic search is configured and has
// vectors to search.
func (s *Service) SemanticEnabled() bool {
	return s.Embed.Enabled() && s.Vectors != nil && s.Vectors.Len() > 0
}

// embedText is what an item is embedded from: the words a person would use
// to describe it, not just its title.
func embedText(r db.ItemsNeedingEmbeddingRow) string {
	var b strings.Builder
	b.WriteString(r.Title)
	if r.Blurb != "" {
		b.WriteString(" — " + r.Blurb)
	}
	fmt.Fprintf(&b, ". A %s", r.Kind)
	if r.Creator != "" {
		b.WriteString(" by " + r.Creator)
	}
	if r.Category != "" {
		b.WriteString(", in " + r.Category)
	}
	b.WriteString(", on " + r.Domain + ".")
	if r.Transcript != "" {
		b.WriteString(" Transcript: " + r.Transcript)
	}
	return b.String()
}

// LoadVectors fills the in-memory index from the database.
func (s *Service) LoadVectors(ctx context.Context) error {
	var after int64
	for {
		rows, err := s.Q.EmbeddingsAfter(ctx, db.EmbeddingsAfterParams{ID: after, Limit: 5000})
		if err != nil {
			return err
		}
		for _, r := range rows {
			s.Vectors.Put(r.ID, embed.FromBytes(r.Embedding))
			after = r.ID
		}
		if len(rows) < 5000 {
			return nil
		}
	}
}

// BackfillEmbeddings embeds up to n live items that have no vector yet,
// best-ranked first. It returns how many it embedded.
func (s *Service) BackfillEmbeddings(ctx context.Context, n int) (int, error) {
	rows, err := s.Q.ItemsNeedingEmbedding(ctx, int32(n))
	if err != nil {
		return 0, err
	}
	done := 0
	for start := 0; start < len(rows); start += 128 {
		batch := rows[start:min(start+128, len(rows))]
		texts := make([]string, len(batch))
		for i, r := range batch {
			texts[i] = embedText(r)
		}
		vecs, err := s.Embed.Embed(ctx, texts, "document")
		if err != nil {
			return done, err
		}
		for i, r := range batch {
			sum := sha256.Sum256([]byte(texts[i]))
			if err := s.Q.SetItemEmbedding(ctx, db.SetItemEmbeddingParams{ID: r.ID, Embedding: embed.Bytes(vecs[i]), EmbeddedHash: sum[:]}); err != nil {
				return done, err
			}
			s.Vectors.Put(r.ID, vecs[i])
			done++
		}
	}
	return done, nil
}

// RunEmbeddings loads the index, then keeps embedding new items: quickly
// while there is a backlog, every few minutes after that, backing off when
// Voyage rate-limits.
func (s *Service) RunEmbeddings(ctx context.Context) {
	if !s.Embed.Enabled() {
		return
	}
	if err := s.LoadVectors(ctx); err != nil {
		s.Log.Error("load vectors", "err", err)
	}
	s.Log.Info("vectors loaded", "count", s.Vectors.Len())
	wait := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		n, err := s.BackfillEmbeddings(ctx, 512)
		switch {
		case err != nil:
			s.Log.Warn("embeddings", "err", err, "embedded", n)
			wait = min(max(wait*2, 30*time.Second), 10*time.Minute)
		case n == 0:
			wait = 3 * time.Minute
		default:
			s.Log.Info("embeddings", "embedded", n, "indexed", s.Vectors.Len())
			wait = 2 * time.Second
		}
	}
}

// queryVector embeds a search query, caching recent queries.
func (s *Service) queryVector(ctx context.Context, q string) ([]int8, error) {
	key := strings.ToLower(strings.TrimSpace(q))
	if v, ok := s.queryCache.Get(key); ok {
		return v, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	vecs, err := s.Embed.Embed(ctx, []string{q}, "query")
	if err != nil {
		return nil, err
	}
	s.queryCache.Set(key, vecs[0])
	return vecs[0], nil
}

const (
	hybridMax  = 200  // results a hybrid search can page through
	minSemanic = 0.25 // cosine floor for meaning-only matches
)

// hybridFeed answers a search by meaning and keywords together: semantic
// neighbours and keyword matches are merged with reciprocal-rank fusion,
// then filtered by the same visibility rules as every feed. Paging uses an
// offset cursor over the fused list.
func (s *Service) hybridFeed(ctx context.Context, viewer *Actor, fq FeedQuery, limit int) (FeedPage, error) {
	var page FeedPage
	offset := 0
	if fq.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(fq.Cursor)
		if err != nil || !strings.HasPrefix(string(b), "h|") {
			return page, errf("invalid", "bad cursor")
		}
		offset, err = strconv.Atoi(strings.TrimPrefix(string(b), "h|"))
		if err != nil || offset < 0 || offset > hybridMax {
			return page, errf("invalid", "bad cursor")
		}
	}
	qv, err := s.queryVector(ctx, fq.Q)
	if err != nil {
		return page, err
	}
	ranks := map[int64]float64{}
	for i, m := range s.Vectors.Nearest(qv, hybridMax, 0) {
		if m.Score < minSemanic {
			break
		}
		ranks[m.ID] += 1.0 / float64(60+i)
	}
	var vid *int64
	if viewer != nil {
		id := viewer.ID()
		vid = &id
	}
	kw, err := s.Q.Feed(ctx, db.FeedParams{Query: fq.Q, Sort: "top", Limit: 100, ViewerID: vid, IncludeDead: fq.IncludeDead})
	if err != nil {
		return page, err
	}
	for i, r := range kw {
		ranks[r.ID] += 1.0 / float64(60+i)
	}
	ids := make([]int64, 0, len(ranks))
	for id := range ranks {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b int64) int {
		switch {
		case ranks[a] > ranks[b]:
			return -1
		case ranks[a] < ranks[b]:
			return 1
		}
		return int(a - b)
	})
	rows, err := s.visibleRows(ctx, viewer, ids, fq.Kind, fq.IncludeDead)
	if err != nil {
		return page, err
	}
	rows = rows[:min(len(rows), hybridMax)]
	offset = min(offset, len(rows))
	end := min(offset+max(limit, 1), len(rows))
	page.Items, err = s.decorate(ctx, viewer, rows[offset:end])
	if err != nil {
		return page, err
	}
	if end < len(rows) {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte("h|" + strconv.Itoa(end)))
	}
	return page, nil
}

// visibleRows loads items by id (keeping the given order) and drops any the
// viewer may not see in a feed.
func (s *Service) visibleRows(ctx context.Context, viewer *Actor, ids []int64, kind string, includeDead bool) ([]db.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.Q.ItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]db.Item, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]db.Item, 0, len(ids))
	for _, id := range ids {
		r, ok := byID[id]
		if !ok || r.Status != "live" || (!includeDead && r.LinkStatus == "dead") || (kind != "" && r.Kind != kind) {
			continue
		}
		if r.CreatedBy != nil {
			if ok, err := s.visibleTo(ctx, viewer, r); err != nil || !ok {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// Similar returns up to n items closest in meaning to an item.
func (s *Service) Similar(ctx context.Context, viewer *Actor, itemID int64, n int) ([]Item, error) {
	if s.Vectors == nil {
		return nil, nil
	}
	v, ok := s.Vectors.Get(itemID)
	if !ok {
		return nil, nil
	}
	matches := s.Vectors.Nearest(v, n*3, itemID)
	ids := make([]int64, 0, len(matches))
	for _, m := range matches {
		if m.Score >= 0.4 {
			ids = append(ids, m.ID)
		}
	}
	rows, err := s.visibleRows(ctx, viewer, ids, "", false)
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, viewer, rows[:min(len(rows), n)])
}
