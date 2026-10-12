package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dogear/internal/db"
)

const (
	// minTopicSim is the cosine similarity an item's embedding needs to the
	// topic's description to be filed by meaning. It is stricter than
	// semantic search's floor (0.25) because topic pages should only hold
	// things clearly about the topic.
	minTopicSim = 0.40
	// maxTopicByMeaning caps how many items one pass files into a topic by
	// meaning, best matches first.
	maxTopicByMeaning = 2000
)

// Topic is a broad, any-medium subject such as AI or History.
type Topic struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Count       int    `json:"count"`
}

// topicCache holds the topic list with counts, which change slowly and
// cost a count per topic to compute.
type topicCache struct {
	mu     sync.Mutex
	at     time.Time
	topics []Topic
	vecs   map[string][]int8 // embedded descriptions, by slug
}

// Topics lists every topic with its live item count, cached for ten minutes.
func (s *Service) Topics(ctx context.Context) ([]Topic, error) {
	s.topics.mu.Lock()
	defer s.topics.mu.Unlock()
	if s.topics.topics != nil && time.Since(s.topics.at) < 10*time.Minute {
		return s.topics.topics, nil
	}
	rows, err := s.Q.ListTopics(ctx)
	if err != nil {
		return nil, fmt.Errorf("topics: %w", err)
	}
	out := make([]Topic, len(rows))
	for i, r := range rows {
		out[i] = Topic{Slug: r.Slug, Name: r.Name, Description: r.Description, Count: int(r.ItemCount)}
	}
	s.topics.topics, s.topics.at = out, time.Now()
	return out, nil
}

// FileTopics files live items into topics: new items by every topic's
// keyword pattern over titles, reasons, and creators (in batches, each item
// once), then, when semantic search is available, the items whose meaning
// is closest to each topic's description. Items are only ever added, so a
// pass is cheap to repeat.
func (s *Service) FileTopics(ctx context.Context) error {
	byKeyword, byMeaning := int64(0), int64(0)
	for {
		n, err := s.Q.FileTopicsByKeywordBatch(ctx, 2000)
		if err != nil {
			return fmt.Errorf("file topics by keyword: %w", err)
		}
		byKeyword += n
		if n == 0 || ctx.Err() != nil {
			break
		}
	}
	topics, err := s.Q.TopicPatterns(ctx)
	if err != nil {
		return fmt.Errorf("topic patterns: %w", err)
	}
	for _, t := range topics {
		if !s.SemanticEnabled() {
			break
		}
		vec, err := s.topicVector(ctx, t.Slug, t.Description)
		if err != nil {
			s.Log.Warn("topic vector", "topic", t.Slug, "err", err)
			continue
		}
		var ids []int64
		for _, m := range s.Vectors.Nearest(vec, maxTopicByMeaning, 0) {
			if m.Score >= minTopicSim {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) > 0 {
			n, err := s.Q.FileTopicByMeaning(ctx, db.FileTopicByMeaningParams{TopicID: t.ID, Ids: ids})
			if err != nil {
				return fmt.Errorf("file %s by meaning: %w", t.Slug, err)
			}
			byMeaning += n
		}
	}
	if byKeyword+byMeaning > 0 {
		s.Log.Info("topics filed", "items_checked", byKeyword, "by_meaning", byMeaning)
		s.topics.mu.Lock()
		s.topics.topics = nil
		s.topics.mu.Unlock()
	}
	return nil
}

// topicVector embeds a topic's description once per process.
func (s *Service) topicVector(ctx context.Context, slug, desc string) ([]int8, error) {
	s.topics.mu.Lock()
	v, ok := s.topics.vecs[slug]
	s.topics.mu.Unlock()
	if ok {
		return v, nil
	}
	vecs, err := s.Embed.Embed(ctx, []string{desc}, "query")
	if err != nil {
		return nil, err
	}
	s.topics.mu.Lock()
	if s.topics.vecs == nil {
		s.topics.vecs = map[string][]int8{}
	}
	s.topics.vecs[slug] = vecs[0]
	s.topics.mu.Unlock()
	return vecs[0], nil
}

// ItemTopics returns the topics an item is filed under.
func (s *Service) ItemTopics(ctx context.Context, itemID int64) []Topic {
	rows, err := s.Q.TopicsForItems(ctx, []int64{itemID})
	if err != nil {
		return nil
	}
	out := make([]Topic, len(rows))
	for i, r := range rows {
		out[i] = Topic{Slug: r.Slug, Name: r.Name}
	}
	return out
}
