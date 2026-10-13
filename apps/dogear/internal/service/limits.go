package service

import (
	"strconv"
	"sync"
	"time"

	"dogear/internal/safety"
)

// Rate-limit rules from the Moderation section, keyed by trust level.
var (
	webPostRules = map[int16][]safety.Rule{
		0: {{Limit: 3, Per: time.Hour}},
		1: {{Limit: 10, Per: time.Hour}, {Limit: 40, Per: 24 * time.Hour}},
	}
	tokenPostRules = map[int16][]safety.Rule{
		0: {{Limit: 3, Per: time.Hour}},
		1: {{Limit: 20, Per: time.Hour}, {Limit: 60, Per: 24 * time.Hour}},
	}
	voteRules = map[int16][]safety.Rule{
		0: {{Limit: 60, Per: time.Minute}},
		1: {{Limit: 120, Per: time.Minute}},
	}
	reportRules = map[int16][]safety.Rule{
		0: {{Limit: 5, Per: time.Hour}},
		1: {{Limit: 10, Per: time.Hour}},
	}
	listCreateRules = []safety.Rule{{Limit: 5, Per: time.Hour}}
	entryRules      = map[int16][]safety.Rule{
		0: {{Limit: 30, Per: time.Hour}},
		1: {{Limit: 120, Per: time.Hour}},
	}
	entryTokenRules = []safety.Rule{{Limit: 60, Per: time.Hour}}
	classifyRules   = []safety.Rule{{Limit: 30, Per: time.Hour}}
	previewRules    = []safety.Rule{{Limit: 30, Per: time.Minute}}
	quickSaveRules  = []safety.Rule{{Limit: 120, Per: time.Hour}}
	groupRules      = []safety.Rule{{Limit: 3, Per: time.Hour}}
	// OAuthCallbackRules applies per client IP to every OAuth callback.
	OAuthCallbackRules = []safety.Rule{{Limit: 10, Per: time.Minute}}
)

func level(l int16) int16 {
	if l >= 1 {
		return 1
	}
	return 0
}

// postKey and postRules pick the bucket for a post: per token for agents,
// per user for the web.
func postKey(a *Actor) (string, []safety.Rule) {
	if a.TokenID != nil {
		return "post:tok:" + strconv.FormatInt(*a.TokenID, 10), tokenPostRules[level(a.User.TrustLevel)]
	}
	return "post:web:" + strconv.FormatInt(a.User.ID, 10), webPostRules[level(a.User.TrustLevel)]
}

// PostRemaining reports how many posts the actor can still make right now.
func (s *Service) PostRemaining(a *Actor) int {
	k, r := postKey(a)
	return s.Limiter.Peek(k, r...)
}

// ttlCache is a small concurrency-safe map whose entries expire, used to
// cache classifier results by normalized URL for 7 days.
type ttlCache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	v   V
	exp time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, m: map[string]ttlEntry[V]{}}
}

func (c *ttlCache[V]) Get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		delete(c.m, k)
		var zero V
		return zero, false
	}
	return e.v, true
}

func (c *ttlCache[V]) Set(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 20000 {
		now := time.Now()
		for key, e := range c.m {
			if now.After(e.exp) {
				delete(c.m, key)
			}
		}
	}
	c.m[k] = ttlEntry[V]{v: v, exp: time.Now().Add(c.ttl)}
}
