package importfile

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func unix(n int64) time.Time { return time.Unix(n, 0).UTC() }

func TestParseFixtures(t *testing.T) {
	tests := []struct {
		file   string
		format Format
		want   []Entry
	}{
		{"pocket.csv", Pocket, []Entry{
			{URL: "https://paulgraham.com/greatwork.html", Title: "How to Do Great Work", Tags: []string{"essays", "career"}, SavedAt: unix(1700000000)},
			{URL: "https://example.com/a?utm_source=pocket", Title: "Comma, in title", SavedAt: unix(1700000100)},
			{URL: "https://example.com/untitled", Title: "https://example.com/untitled", Tags: []string{"reading"}, SavedAt: unix(1700000200)},
		}},
		{"pocket_reordered.csv", Pocket, []Entry{
			{URL: "https://example.com/b", Title: "Bee", Tags: []string{"a", "b"}, SavedAt: unix(1700000300)},
		}},
		{"raindrop.csv", Raindrop, []Entry{
			{URL: "https://go-proverbs.github.io/", Title: "Go Proverbs", Note: "simple beats clever", Tags: []string{"Dev", "Go", "go", "talks"},
				SavedAt: time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)},
			{URL: "https://example.com/x", Title: "Unsorted thing", SavedAt: time.Date(2024, 3, 2, 10, 0, 0, 0, time.UTC)},
		}},
		{"bookmarks.html", Netscape, []Entry{
			{URL: "https://news.ycombinator.com/", Title: "Hacker News", SavedAt: unix(1600000001)},
			{URL: "https://example.com/carbonara", Title: "Carbonara & friends", Note: "The real one, no cream.",
				Tags: []string{"Recipes", "Italian", "pasta", "dinner"}, SavedAt: unix(1600000003)},
			{URL: "javascript:alert(1)", Title: "bookmarklet"},
			{URL: "https://pinboard.in/u:someone", Title: "Pinboard", Tags: []string{"tools"}, SavedAt: unix(1600000004)},
		}},
		{"Liked videos.csv", YouTube, []Entry{
			{URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Tags: []string{"liked videos"}, SavedAt: time.Date(2023, 5, 1, 12, 0, 0, 0, time.UTC)},
			{URL: "https://www.youtube.com/watch?v=jNQXAC9IVRw", Tags: []string{"liked videos"}, SavedAt: time.Date(2023, 5, 2, 12, 0, 0, 0, time.UTC)},
		}},
		{"Road trip-videos.csv", YouTube, []Entry{
			{URL: "https://www.youtube.com/watch?v=9bZkp7q19f0", Tags: []string{"road trip"}, SavedAt: time.Date(2020, 1, 1, 0, 0, 1, 0, time.UTC)},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			res, err := Parse(tt.file, bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if res.Format != tt.format {
				t.Errorf("format = %q, want %q", res.Format, tt.format)
			}
			if !reflect.DeepEqual(res.Entries, tt.want) {
				t.Errorf("entries:\n got %#v\nwant %#v", res.Entries, tt.want)
			}
		})
	}
}

func TestParseZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, fixture string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}
	add("Takeout/YouTube and YouTube Music/playlists/Liked videos.csv", "Liked videos.csv")
	// Not exports: skipped without failing the upload.
	w, _ := zw.Create("Takeout/YouTube and YouTube Music/history/watch-history.html")
	w.Write([]byte(`<html><body><a href="https://www.youtube.com/watch?v=x">x</a></body></html>`))
	w, _ = zw.Create("Takeout/archive_browser.html")
	w.Write([]byte(`<p>hello</p>`))
	zw.Close()

	res, err := Parse("takeout.zip", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != YouTube || len(res.Entries) != 2 || res.Entries[0].Tags[0] != "liked videos" {
		t.Fatalf("got %q %#v", res.Format, res.Entries)
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse("notes.txt", strings.NewReader("just some text\nwith lines")); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("plain text: err = %v, want ErrUnknownFormat", err)
	}
	if _, err := Parse("big.csv", bytes.NewReader(make([]byte, MaxBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize: err = %v, want ErrTooLarge", err)
	}
	var sb strings.Builder
	sb.WriteString("url\n")
	for range MaxEntries + 5 {
		sb.WriteString("https://example.com/\n")
	}
	res, err := Parse("many.csv", strings.NewReader(sb.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != MaxEntries || !res.Truncated {
		t.Errorf("got %d entries, truncated %v", len(res.Entries), res.Truncated)
	}
}

func TestParseGenericCSV(t *testing.T) {
	in := "\xef\xbb\xbfName,Link,Description,Tags\nA,https://a.example/,why it matters,\"x, y\"\n,,,\n"
	res, err := Parse("export.csv", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{URL: "https://a.example/", Title: "A", Note: "why it matters", Tags: []string{"x", "y"}}}
	if res.Format != CSV || !reflect.DeepEqual(res.Entries, want) {
		t.Fatalf("got %q %#v", res.Format, res.Entries)
	}
}

func TestParseTime(t *testing.T) {
	tests := map[string]time.Time{
		"1700000000":                unix(1700000000),
		"1700000000000":             unix(1700000000),
		"1700000000000000":          unix(1700000000),
		"2024-03-01":                time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		"2024-03-01T10:00:00+02:00": time.Date(2024, 3, 1, 8, 0, 0, 0, time.UTC),
		"0":                         {},
		"9999999999":                {}, // in the future
		"yesterday":                 {},
	}
	for in, want := range tests {
		if got := parseTime(in); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want %v", in, got, want)
		}
	}
}
