package safety

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic limiter tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter() (*Limiter, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := NewLimiter()
	l.now = clock.now
	return l, clock
}

func TestLimiterAllowSequence(t *testing.T) {
	hourly := Rule{Limit: 3, Per: time.Hour}
	type step struct {
		advance   time.Duration
		ok        bool
		remaining int
		retry     time.Duration
	}
	tests := []struct {
		name  string
		rules []Rule
		steps []step
	}{
		{
			name:  "exhaust then refill one unit",
			rules: []Rule{hourly},
			steps: []step{
				{0, true, 2, 0},
				{0, true, 1, 0},
				{0, true, 0, 0},
				{0, false, 0, 20 * time.Minute},
				{10 * time.Minute, false, 0, 10 * time.Minute},
				{10 * time.Minute, true, 0, 0},
				{0, false, 0, 20 * time.Minute},
			},
		},
		{
			name:  "full refill caps at limit",
			rules: []Rule{hourly},
			steps: []step{
				{0, true, 2, 0},
				{5 * time.Hour, true, 2, 0},
			},
		},
		{
			name:  "two rules, tighter one blocks",
			rules: []Rule{{Limit: 10, Per: time.Hour}, {Limit: 2, Per: time.Minute}},
			steps: []step{
				{0, true, 1, 0},
				{0, true, 0, 0},
				{0, false, 0, 30 * time.Second},
				{30 * time.Second, true, 0, 0},
			},
		},
		{
			name:  "day rule outlasts hour rule",
			rules: []Rule{{Limit: 2, Per: time.Hour}, {Limit: 3, Per: 24 * time.Hour}},
			steps: []step{
				{0, true, 1, 0},
				{0, true, 0, 0},
				{time.Hour, true, 0, 0}, // hour bucket back to 2, day bucket 1 -> 0
				// Day bucket refilled 1/8 unit in that hour and is at 0.125 after
				// the call, so the day rule needs 0.875 * 8h = 7h for one unit.
				{0, false, 0, 7 * time.Hour},
			},
		},
		{
			name:  "invalid rule always blocks",
			rules: []Rule{{Limit: 0, Per: time.Minute}},
			steps: []step{{0, false, 0, time.Minute}},
		},
		{
			name:  "no rules always allowed",
			rules: nil,
			steps: []step{{0, true, 0, 0}, {0, true, 0, 0}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, clock := newTestLimiter()
			for i, s := range tt.steps {
				clock.advance(s.advance)
				ok, rem, retry := l.Allow("user:1", tt.rules...)
				if ok != s.ok || rem != s.remaining || retry != s.retry {
					t.Fatalf("step %d: Allow = (%v, %d, %v), want (%v, %d, %v)", i, ok, rem, retry, s.ok, s.remaining, s.retry)
				}
			}
		})
	}
}

func TestLimiterBlockedConsumesNothing(t *testing.T) {
	l, _ := newTestLimiter()
	loose := Rule{Limit: 10, Per: time.Hour}
	tight := Rule{Limit: 1, Per: time.Hour}
	if ok, _, _ := l.Allow("k", loose, tight); !ok {
		t.Fatal("first call blocked")
	}
	for range 5 {
		if ok, _, _ := l.Allow("k", loose, tight); ok {
			t.Fatal("tight rule did not block")
		}
	}
	// The loose bucket was charged only once.
	if got := l.Peek("k", loose); got != 9 {
		t.Fatalf("loose remaining = %d, want 9", got)
	}
}

func TestLimiterKeysAndRulesIndependent(t *testing.T) {
	l, _ := newTestLimiter()
	r := Rule{Limit: 1, Per: time.Hour}
	if ok, _, _ := l.Allow("a", r); !ok {
		t.Fatal("a blocked")
	}
	if ok, _, _ := l.Allow("b", r); !ok {
		t.Fatal("b blocked by a's bucket")
	}
	// Same key, different rule = different bucket.
	if ok, _, _ := l.Allow("a", Rule{Limit: 1, Per: time.Minute}); !ok {
		t.Fatal("a/minute blocked by a/hour bucket")
	}
	if ok, _, _ := l.Allow("a", r); ok {
		t.Fatal("a/hour should be exhausted")
	}
}

func TestLimiterPeek(t *testing.T) {
	l, clock := newTestLimiter()
	r := Rule{Limit: 5, Per: 5 * time.Minute}
	if got := l.Peek("k", r); got != 5 {
		t.Fatalf("Peek unseen = %d, want 5", got)
	}
	l.Allow("k", r)
	l.Allow("k", r)
	if got := l.Peek("k", r); got != 3 {
		t.Fatalf("Peek = %d, want 3", got)
	}
	if got := l.Peek("k", r); got != 3 {
		t.Fatal("Peek consumed a unit")
	}
	clock.advance(90 * time.Second) // +1.5 units
	if got := l.Peek("k", r); got != 4 {
		t.Fatalf("Peek after refill = %d, want 4", got)
	}
	if got := l.Peek("k", r, Rule{Limit: 2, Per: time.Hour}); got != 2 {
		t.Fatalf("Peek min across rules = %d, want 2", got)
	}
	if got := l.Peek("k"); got != 0 {
		t.Fatalf("Peek no rules = %d, want 0", got)
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	l, clock := newTestLimiter()
	r := Rule{Limit: 2, Per: time.Minute}
	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k, r)
	}
	if len(l.buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(l.buckets))
	}
	clock.advance(2 * time.Minute)
	l.Allow("d", r) // triggers a sweep: a, b, c are full again
	if len(l.buckets) != 1 {
		t.Fatalf("after sweep buckets = %d, want 1", len(l.buckets))
	}
	// A partially drained bucket survives the sweep.
	long := Rule{Limit: 2, Per: time.Hour}
	l.Allow("e", long)
	clock.advance(sweepInterval)
	l.Peek("x", r)
	if _, ok := l.buckets[bucketKey{"e", 2, time.Hour}]; !ok {
		t.Fatal("partially drained bucket was swept")
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter()
	r := Rule{Limit: 100, Per: time.Hour}
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if ok, _, _ := l.Allow("shared", r); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if allowed != 100 {
		t.Fatalf("allowed = %d, want exactly 100", allowed)
	}
}
