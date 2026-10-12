package service

import (
	"context"
	"fmt"
	"testing"

	"dogear/internal/db"
)

func TestFileTopicsByKeywordAndFeed(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	poster := newUser(t, s, 1)
	seq++
	u := fmt.Sprintf("https://example%d.test/llm-primer", seq)
	ai, err := s.Q.InsertItem(ctx, db.InsertItemParams{Url: u, UrlNorm: u, Domain: "example.test",
		Title: "Intro to Large Language Models", Blurb: "A one-hour primer on how LLMs work.",
		CreatedBy: &poster.User.ID, Via: "web", Status: "live", Kind: "video", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	other := liveItem(t, s, poster) // "Thing — A thing worth a look." matches no topic
	if err := s.FileTopics(ctx); err != nil {
		t.Fatal(err)
	}
	got := s.ItemTopics(ctx, ai.ID)
	if len(got) == 0 || got[0].Slug != "ai" {
		t.Fatalf("AI item topics = %+v", got)
	}
	if ts := s.ItemTopics(ctx, other); len(ts) != 0 {
		t.Fatalf("plain item filed under %+v", ts)
	}
	page, err := s.Feed(ctx, nil, FeedQuery{Topic: "ai", Sort: "new", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range page.Items {
		found = found || it.ID == ai.ID
		if it.ID == other {
			t.Fatal("topic feed holds an item outside the topic")
		}
	}
	if !found || page.Topic == nil || page.Topic.Name != "AI" {
		t.Fatalf("topic feed missing the AI item (topic %+v)", page.Topic)
	}
	if _, err := s.Feed(ctx, nil, FeedQuery{Topic: "nope"}); err != ErrNotFound {
		t.Fatalf("unknown topic: %v", err)
	}
}
