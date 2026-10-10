package safety

import (
	"math"
	"sync"
	"time"
)

// Rule is one rate limit: at most Limit units per Per, as a token bucket with
// capacity Limit that refills continuously at Limit tokens per Per.
type Rule struct {
	Limit int
	Per   time.Duration
}

// bucketKey identifies one bucket: a caller key under one specific rule, so
// "10 per hour" and "40 per day" for the same user are tracked separately.
type bucketKey struct {
	key   string
	limit int
	per   time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

// sweepInterval is how often Allow/Peek scan for idle buckets to delete.
const sweepInterval = time.Minute

// Limiter is an in-memory set of token buckets, safe for concurrent use.
// Idle buckets (fully refilled, so indistinguishable from new ones) are
// removed lazily during calls, so no background goroutine is needed.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[bucketKey]*bucket
	lastSweep time.Time
	now       func() time.Time // overridable in tests
}

// NewLimiter returns an empty Limiter.
func NewLimiter() *Limiter {
	return &Limiter{buckets: make(map[bucketKey]*bucket), now: time.Now}
}

// refill returns b's token count at time now without mutating b. A missing
// bucket is full.
func refill(b *bucket, r Rule, now time.Time) float64 {
	capacity := float64(r.Limit)
	if b == nil {
		return capacity
	}
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return b.tokens
	}
	tokens := b.tokens + float64(elapsed)/float64(r.Per)*capacity
	// Round away float noise so exactly one refill interval yields a whole token.
	tokens = math.Round(tokens*1e9) / 1e9
	return math.Min(tokens, capacity)
}

// validRule reports whether a rule can ever admit a request.
func validRule(r Rule) bool { return r.Limit > 0 && r.Per > 0 }

// Allow consumes one unit from every rule's bucket for key. If any rule is
// exhausted, nothing is consumed, ok is false, and retryAfter is how long until
// the most-blocking rule has a unit again. remaining is the smallest number of
// units left across the rules after the call. With no rules, Allow always
// succeeds and reports remaining 0.
func (l *Limiter) Allow(key string, rules ...Rule) (ok bool, remaining int, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.maybeSweep(now)

	if len(rules) == 0 {
		return true, 0, 0
	}
	levels := make([]float64, len(rules))
	blocked := false
	for i, r := range rules {
		if !validRule(r) {
			blocked = true
			retryAfter = max(retryAfter, max(r.Per, time.Second))
			continue
		}
		levels[i] = refill(l.buckets[bucketKey{key, r.Limit, r.Per}], r, now)
		if levels[i] < 1 {
			blocked = true
			// Time for the deficit to refill at Limit tokens per Per.
			wait := time.Duration(math.Ceil((1 - levels[i]) * float64(r.Per) / float64(r.Limit)))
			retryAfter = max(retryAfter, wait)
		}
	}
	if blocked {
		return false, 0, retryAfter
	}

	remaining = math.MaxInt
	for i, r := range rules {
		k := bucketKey{key, r.Limit, r.Per}
		b := l.buckets[k]
		if b == nil {
			b = &bucket{}
			l.buckets[k] = b
		}
		b.tokens = levels[i] - 1
		b.last = now
		remaining = min(remaining, int(math.Floor(b.tokens)))
	}
	return true, remaining, 0
}

// Peek reports the smallest number of whole units available across rules for
// key, without consuming anything.
func (l *Limiter) Peek(key string, rules ...Rule) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.maybeSweep(now)

	if len(rules) == 0 {
		return 0
	}
	remaining := math.MaxInt
	for _, r := range rules {
		if !validRule(r) {
			return 0
		}
		level := refill(l.buckets[bucketKey{key, r.Limit, r.Per}], r, now)
		remaining = min(remaining, int(math.Floor(level)))
	}
	return remaining
}

// maybeSweep deletes buckets that have fully refilled, at most once per
// sweepInterval, so memory stays bounded by recently active keys. Caller must
// hold l.mu.
func (l *Limiter) maybeSweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if refill(b, Rule{Limit: k.limit, Per: k.per}, now) >= float64(k.limit) {
			delete(l.buckets, k)
		}
	}
}
