package importer

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var update = flag.Bool("update", false, "regenerate testdata/*.golden.json")

// goldenResult is the on-disk shape of a ParseList golden file.
type goldenResult struct {
	Items []ParsedItem `json:"items"`
	Skips SkipStats    `json:"skips"`
}

func TestParseListGolden(t *testing.T) {
	for _, name := range []string{"awesome-go", "awesome-nodejs", "awesome-python"} {
		t.Run(name, func(t *testing.T) {
			md, err := os.ReadFile(filepath.Join("testdata", name+".md"))
			if err != nil {
				t.Fatal(err)
			}
			items, skips := ParseList(md)
			got, err := json.MarshalIndent(goldenResult{Items: items, Skips: skips}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("ParseList(%s) differs from %s; run go test -update and review the diff", name, golden)
			}
		})
	}
}

func TestParseIndex(t *testing.T) {
	md, err := os.ReadFile(filepath.Join("testdata", "index.md"))
	if err != nil {
		t.Fatal(err)
	}
	cats := ParseIndex(md)
	if len(cats) <= 20 {
		t.Errorf("got %d categories, want > 20", len(cats))
	}
	total := 0
	found := false
	for _, c := range cats {
		switch c.Name {
		case "Contributing", "Contents", "Related", "License", "Footnotes", "Conventions":
			t.Errorf("unexpected category %q", c.Name)
		}
		total += len(c.Lists)
		for _, l := range c.Lists {
			if l.Repo == "" || l.Title == "" {
				t.Errorf("incomplete list %+v in %q", l, c.Name)
			}
			if c.Name == "Programming Languages" && l.Repo == "avelino/awesome-go" {
				found = true
			}
		}
	}
	t.Logf("index: %d categories, %d lists", len(cats), total)
	if total <= 400 {
		t.Errorf("got %d lists, want > 400", total)
	}
	if !found {
		t.Error("avelino/awesome-go not found under Programming Languages")
	}
}

func TestParseIndexSynthetic(t *testing.T) {
	md := `# Awesome

## Contents

- [Platforms](#platforms)

## Platforms

- [Node.js](https://github.com/sindresorhus/awesome-nodejs#readme) - Async non-blocking event-driven JavaScript runtime.
	- [Cross-Platform](https://github.com/bcoe/awesome-cross-platform-nodejs#readme) - Writing cross-platform code.
- [Not a repo](https://github.com/topics/awesome)
- [Gitlab](https://gitlab.com/x/y)
- [Deep](https://github.com/a/b/tree/main/docs)

## Contributing

- [Guide](https://github.com/sindresorhus/awesome/blob/main/contributing.md)
- [Other](https://github.com/x/awesome-x)
`
	got := ParseIndex([]byte(md))
	want := []IndexCategory{{
		Name: "Platforms",
		Lists: []IndexList{
			{Repo: "sindresorhus/awesome-nodejs", Title: "Node.js", Description: "Async non-blocking event-driven JavaScript runtime."},
			{Repo: "bcoe/awesome-cross-platform-nodejs", Title: "Cross-Platform", Description: "Writing cross-platform code."},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseIndex = %+v\nwant %+v", got, want)
	}
}

func TestParseListCases(t *testing.T) {
	cases := []struct {
		name  string
		md    string
		items []ParsedItem
		skips SkipStats
	}{
		{
			name: "description separators",
			md: "## Tools\n\n" +
				"- [A](https://a.dev) - hyphen desc\n" +
				"- [B](https://b.dev) – en dash desc\n" +
				"- [C](https://c.dev) — em dash desc\n" +
				"- [D](https://d.dev): colon desc\n" +
				"- [E](https://e.dev)\n" +
				"- **[F](https://f.dev)** - _emphasis_ and `code` and [link](https://x.dev)\n",
			items: []ParsedItem{
				{Title: "A", URL: "https://a.dev", Description: "hyphen desc", HeadingPath: "Tools"},
				{Title: "B", URL: "https://b.dev", Description: "en dash desc", HeadingPath: "Tools"},
				{Title: "C", URL: "https://c.dev", Description: "em dash desc", HeadingPath: "Tools"},
				{Title: "D", URL: "https://d.dev", Description: "colon desc", HeadingPath: "Tools"},
				{Title: "E", URL: "https://e.dev", HeadingPath: "Tools"},
				{Title: "F", URL: "https://f.dev", Description: "emphasis and code and link", HeadingPath: "Tools"},
			},
			skips: SkipStats{},
		},
		{
			name: "nested lists and heading stack",
			md: "# Title\n\n## Web\n\n### Frameworks\n\n#### Small\n\n##### Ignored level\n\n" +
				"- Group without link\n" +
				"  - [Child](https://child.dev) - nested one\n" +
				"    - [Grandchild](http://gc.dev) - nested two\n" +
				"- [Parent](https://parent.dev) - top\n\n" +
				"## Other\n\n- [O](https://o.dev)\n",
			items: []ParsedItem{
				{Title: "Child", URL: "https://child.dev", Description: "nested one", HeadingPath: "Web > Frameworks > Small"},
				{Title: "Grandchild", URL: "http://gc.dev", Description: "nested two", HeadingPath: "Web > Frameworks > Small"},
				{Title: "Parent", URL: "https://parent.dev", Description: "top", HeadingPath: "Web > Frameworks > Small"},
				{Title: "O", URL: "https://o.dev", HeadingPath: "Other"},
			},
			skips: SkipStats{"no_link": 1},
		},
		{
			name: "anchor heavy section is a TOC",
			md: "## Index\n\n- [One](#one)\n- [Two](#two)\n- [Three](https://three.dev)\n\n" +
				"## One\n\n- [Real](https://real.dev) - ok\n- [Jump](#two)\n",
			items: []ParsedItem{
				{Title: "Real", URL: "https://real.dev", Description: "ok", HeadingPath: "One"},
			},
			skips: SkipStats{"anchor_section": 3, "anchor": 1},
		},
		{
			name: "skipped headings",
			md: "## 📖 Table of Contents:\n\n- [Thing](https://thing.dev)\n\n" +
				"## Contributing\n\n### Guide\n\n- [Guide](https://guide.dev)\n\n" +
				"## LICENSE\n\n- [CC0](https://creativecommons.org)\n\n" +
				"## Related Lists\n\n- [awesome-x](https://github.com/x/awesome-x)\n\n" +
				"## Kept\n\n- [K](https://k.dev)\n",
			items: []ParsedItem{{Title: "K", URL: "https://k.dev", HeadingPath: "Kept"}},
			skips: SkipStats{"skipped_heading": 4},
		},
		{
			name: "badge image mailto relative scheme",
			md: "## Misc\n\n" +
				"- [![Build](https://travis-ci.org/a/b.svg)](https://travis-ci.org/a/b)\n" +
				"- [shield](https://img.shields.io/badge/x-y-green)\n" +
				"- [![logo](https://example.com/logo.png)](https://example.com)\n" +
				"- ![just an image](https://example.com/pic.png)\n" +
				"- [Mail me](mailto:me@example.com)\n" +
				"- [Local](docs/README.md)\n" +
				"- [FTP](ftp://files.example.com)\n" +
				"- [](https://empty.dev)\n" +
				"- [Good](https://good.dev) - with ![badge](https://img.shields.io/x) badge\n",
			items: []ParsedItem{
				{Title: "Good", URL: "https://good.dev", Description: "with badge", HeadingPath: "Misc"},
			},
			skips: SkipStats{"badge": 2, "image": 2, "mailto": 1, "relative": 1, "bad_scheme": 1, "no_link": 1},
		},
		{
			name: "multiple lists under one heading are separate sections",
			md:   "## Mixed\n\n**TOC**\n\n- [A](#a)\n- [B](#b)\n\n**Real**\n\n- [R](https://r.dev) - real\n",
			items: []ParsedItem{
				{Title: "R", URL: "https://r.dev", Description: "real", HeadingPath: "Mixed"},
			},
			skips: SkipStats{"anchor_section": 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, skips := ParseList([]byte(tc.md))
			if !reflect.DeepEqual(items, tc.items) {
				t.Errorf("items:\n got %+v\nwant %+v", items, tc.items)
			}
			if !reflect.DeepEqual(skips, tc.skips) {
				t.Errorf("skips: got %v want %v", skips, tc.skips)
			}
		})
	}
}

func TestTruncation(t *testing.T) {
	long := "word "
	for len(long) < 400 {
		long += "word "
	}
	items, _ := ParseList([]byte("## X\n\n- [" + long + "](https://x.dev) - " + long + "\n"))
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	if n := len([]rune(items[0].Title)); n > maxTitleLen {
		t.Errorf("title has %d runes", n)
	}
	if n := len([]rune(items[0].Description)); n > maxDescriptionLen {
		t.Errorf("description has %d runes", n)
	}
	if got := items[0].Description; got[len(got)-len("word…"):] != "word…" {
		t.Errorf("description not cut at word boundary: %q", got[len(got)-10:])
	}
}
