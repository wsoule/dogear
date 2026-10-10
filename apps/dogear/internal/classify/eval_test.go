package classify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const evalPath = "../../testdata/classify/eval.jsonl"

// evalExample is one labeled line of testdata/classify/eval.jsonl.
type evalExample struct {
	URL             string `json:"url"`
	Title           string `json:"title"`
	Blurb           string `json:"blurb"`
	PageTitle       string `json:"page_title"`
	MetaDescription string `json:"meta_description"`
	PageText        string `json:"page_text"`
	Expect          struct {
		Kind      string `json:"kind"`
		Category  string `json:"category"`
		Promo     bool   `json:"promo"`
		Injection bool   `json:"injection"`
		Mismatch  bool   `json:"mismatch"`
		Unsafe    bool   `json:"unsafe"`
	} `json:"expect"`
}

// evalCategories mirrors the seeded leaf categories (migration 00004),
// minus the hidden "unsorted" bucket.
var evalCategories = []Category{
	{"computers", "Computers", "Laptops, desktops, single-board computers, and repair"},
	{"phones", "Phones", "Smartphones and phone accessories"},
	{"audio", "Audio", "Headphones, speakers, and audio software"},
	{"peripherals", "Peripherals", "Keyboards, mice, monitors, and other input/output devices"},
	{"wearables", "Wearables", "Smartwatches, fitness trackers, and other wearable devices"},
	{"mac", "Mac", "Apps and utilities for macOS"},
	{"windows", "Windows", "Apps and utilities for Windows"},
	{"linux-apps", "Linux", "Apps and app stores for desktop Linux"},
	{"ios", "iOS", "Apps for iPhone and iPad"},
	{"android", "Android", "Apps and app stores for Android"},
	{"web-apps", "Web apps", "Tools that run in the browser"},
	{"command-line", "Command line", "Terminal programs and shell tools"},
	{"note-taking", "Note-taking", "Note apps, outliners, and personal knowledge bases"},
	{"automation", "Automation", "Workflow, macro, and home automation"},
	{"focus", "Focus", "Timers, blockers, and tools for concentration"},
	{"learn-programming", "Programming", "Learning to code: books, courses, talks, articles"},
	{"learn-design", "Design", "Learning design: UX, typography, visual design"},
	{"learn-science", "Science", "Learning math, physics, and other sciences"},
	{"learn-business", "Business", "Startups, strategy, and business writing"},
}

// loadEval reads every labeled example.
func loadEval(t *testing.T) []evalExample {
	t.Helper()
	f, err := os.Open(evalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []evalExample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var e evalExample
		dec := json.NewDecoder(strings.NewReader(sc.Text()))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("%s:%d: %v", evalPath, n, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEvalData checks the eval file is well formed, so it stays usable even
// when no API key is around to run TestEval.
func TestEvalData(t *testing.T) {
	ex := loadEval(t)
	if len(ex) < 90 {
		t.Errorf("only %d eval examples", len(ex))
	}
	cats := map[string]bool{}
	for _, c := range evalCategories {
		cats[c.Slug] = true
	}
	for i, e := range ex {
		if !IsKind(e.Expect.Kind) {
			t.Errorf("example %d: bad kind %q", i+1, e.Expect.Kind)
		}
		if e.Expect.Category != "" && !cats[e.Expect.Category] {
			t.Errorf("example %d: unknown category %q", i+1, e.Expect.Category)
		}
		if e.URL == "" || e.Title == "" {
			t.Errorf("example %d: missing url or title", i+1)
		}
	}
}

// TestEval runs the live Jev classifier over the eval set and prints
// precision/recall per signal at several thresholds, plus kind and category
// accuracy. It is skipped unless JEV_API_KEY is set (JEV_BASE_URL optional):
//
//	JEV_API_KEY=... go test ./internal/classify -run Eval -v
func TestEval(t *testing.T) {
	key := os.Getenv("JEV_API_KEY")
	if key == "" {
		t.Skip("JEV_API_KEY not set")
	}
	ex := loadEval(t)
	j := &Jev{APIKey: key, BaseURL: os.Getenv("JEV_BASE_URL")}

	results := make([]Result, len(ex))
	errs := make([]error, len(ex))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	start := time.Now()
	for i, e := range ex {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			results[i], errs[i] = j.Classify(ctx, Input{
				URL: e.URL, Title: e.Title, Blurb: e.Blurb, PageTitle: e.PageTitle,
				MetaDescription: e.MetaDescription, PageText: e.PageText, Categories: evalCategories,
			})
		}()
	}
	wg.Wait()
	nErr := 0
	for i, err := range errs {
		if err != nil {
			nErr++
			t.Logf("example %d (%s): %v", i+1, ex[i].URL, err)
		}
	}
	fmt.Printf("eval: %d examples, %d errors, %v\n", len(ex), nErr, time.Since(start).Round(time.Millisecond))

	signals := []struct {
		name  string
		score func(Result) float64
		want  func(evalExample) bool
	}{
		{"promo", func(r Result) float64 { return r.PromoConf }, func(e evalExample) bool { return e.Expect.Promo }},
		{"injection", func(r Result) float64 { return r.InjectionConf }, func(e evalExample) bool { return e.Expect.Injection }},
		{"mismatch", func(r Result) float64 { return r.MismatchConf }, func(e evalExample) bool { return e.Expect.Mismatch }},
		{"unsafe", func(r Result) float64 { return r.UnsafeConf }, func(e evalExample) bool { return e.Expect.Unsafe }},
	}
	thresholds := []float64{0.5, 0.6, 0.7, 0.8, 0.9, 0.95}
	fmt.Printf("%-10s %5s %4s %4s %4s %9s %7s\n", "signal", "thr", "tp", "fp", "fn", "precision", "recall")
	for _, s := range signals {
		for _, th := range thresholds {
			var tp, fp, fn int
			for i, e := range ex {
				if errs[i] != nil {
					continue
				}
				got, want := s.score(results[i]) >= th, s.want(e)
				switch {
				case got && want:
					tp++
				case got && !want:
					fp++
				case !got && want:
					fn++
				}
			}
			fmt.Printf("%-10s %5.2f %4d %4d %4d %9s %7s\n", s.name, th, tp, fp, fn, ratio(tp, tp+fp), ratio(tp, tp+fn))
		}
	}

	// Kind accuracy: raw classifier answer, and KindFor with no poster kind
	// (domain rules + 0.6 threshold) as the site would assign it.
	var n, rawOK, finalOK, catN, catOK int
	for i, e := range ex {
		if errs[i] != nil {
			continue
		}
		n++
		if results[i].Kind == e.Expect.Kind {
			rawOK++
		}
		if KindFor("", e.URL, results[i]) == e.Expect.Kind {
			finalOK++
		} else {
			t.Logf("kind miss %s: want %s got %s (%.2f)", e.URL, e.Expect.Kind, results[i].Kind, results[i].KindConf)
		}
		if e.Expect.Category != "" {
			catN++
			if results[i].CategorySlug == e.Expect.Category {
				catOK++
			}
		}
	}
	fmt.Printf("kind accuracy: classifier %s, KindFor %s (n=%d)\n", ratio(rawOK, n), ratio(finalOK, n), n)
	fmt.Printf("category accuracy: %s (n=%d)\n", ratio(catOK, catN), catN)
}

// ratio formats a/b as a fraction, or "-" when b is zero.
func ratio(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", float64(a)/float64(b))
}
