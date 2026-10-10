package importer

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// IndexList is one awesome list linked from the sindresorhus/awesome index.
type IndexList struct {
	Repo        string // "owner/name", lowercased
	Title       string // link text
	Description string
}

// IndexCategory is one H2 section of the index README with the lists under it.
type IndexCategory struct {
	Name  string // H2 text
	Lists []IndexList
}

// ParsedItem is one entry extracted from an awesome list README.
type ParsedItem struct {
	Title       string `json:"title"`
	URL         string `json:"url"` // absolute http(s) as written
	Description string `json:"description,omitempty"`
	HeadingPath string `json:"heading_path"` // "A > B" from the H2..H4 stack
}

// SkipStats counts skipped list items by reason. Reasons: "relative",
// "anchor", "mailto", "image", "badge", "skipped_heading", "anchor_section",
// "no_link", "bad_scheme".
type SkipStats map[string]int

const (
	maxTitleLen       = 120
	maxDescriptionLen = 280
)

// listSkipHeadings are headings (normalized) whose list items are never items.
var listSkipHeadings = map[string]bool{
	"contents":          true,
	"table of contents": true,
	"contributing":      true,
	"license":           true,
	"footnotes":         true,
	"related":           true,
}

// indexSkipHeadings are H2 sections of the index README that are not categories.
var indexSkipHeadings = map[string]bool{
	"contributing": true,
	"license":      true,
	"conventions":  true,
	"contents":     true,
	"related":      true,
	"footnotes":    true,
}

// badgeHosts serve status/badge images; links or images pointing at them are
// decoration, not list entries.
var badgeHosts = map[string]bool{
	"shields.io":        true,
	"img.shields.io":    true,
	"badgen.net":        true,
	"flat.badgen.net":   true,
	"badge.fury.io":     true,
	"travis-ci.org":     true,
	"travis-ci.com":     true,
	"api.travis-ci.org": true,
	"api.travis-ci.com": true,
	"ci.appveyor.com":   true,
	"coveralls.io":      true,
	"codecov.io":        true,
	"david-dm.org":      true,
	"circleci.com":      true,
	"app.circleci.com":  true,
	"img.badgesize.io":  true,
	"badges.gitter.im":  true,
	"api.codacy.com":    true,
	"app.codacy.com":    true,
	"awesome.re":        true,
	"sonarcloud.io":     true,
	"api.netlify.com":   true,
	"deepsource.io":     true,
	"bestpractices.dev": true,
}

// parseMarkdown parses a README with GitHub-flavored extensions so bare URLs,
// tables and strikethrough match what github.com renders.
func parseMarkdown(md []byte) ast.Node {
	p := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()
	return p.Parse(text.NewReader(md))
}

// ParseIndex parses the sindresorhus/awesome README. Each H2 is a top-level
// category and each list item under it (nested items included) whose first
// link points at a github.com/{owner}/{repo} root is an awesome list.
func ParseIndex(md []byte) []IndexCategory {
	src := md
	doc := parseMarkdown(md)
	var cats []IndexCategory
	var cur *IndexCategory
	seen := map[string]bool{}
	skipping := true // content before the first H2 is the header, not a category

	var walkList func(list ast.Node)
	walkList = func(list ast.Node) {
		for li := list.FirstChild(); li != nil; li = li.NextSibling() {
			ex := extractItem(li, src)
			if ex.link != nil {
				if repo, ok := githubRepoRoot(ex.dest); ok && !seen[repo] {
					seen[repo] = true
					cur.Lists = append(cur.Lists, IndexList{
						Repo:        repo,
						Title:       truncateWords(ex.title, maxTitleLen),
						Description: cleanDescription(ex.after.String()),
					})
				}
			}
			for c := li.FirstChild(); c != nil; c = c.NextSibling() {
				if c.Kind() == ast.KindList {
					walkList(c)
				}
			}
		}
	}

	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c.Kind() {
			case ast.KindHeading:
				h := c.(*ast.Heading)
				if h.Level == 1 {
					skipping, cur = true, nil
				}
				if h.Level != 2 {
					continue
				}
				name := collapseSpace(plainText(h, src))
				if indexSkipHeadings[normalizeHeading(name)] {
					skipping, cur = true, nil
					continue
				}
				skipping = false
				cats = append(cats, IndexCategory{Name: name})
				cur = &cats[len(cats)-1]
				seen = map[string]bool{}
			case ast.KindList:
				if !skipping && cur != nil {
					walkList(c)
				}
			case ast.KindBlockquote:
				walk(c)
			}
		}
	}
	walk(doc)

	out := cats[:0]
	for _, c := range cats {
		if len(c.Lists) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// githubRepoRoot reports whether u is a link to a GitHub repository root
// (github.com/{owner}/{repo}, optionally with a fragment such as #readme or
// a trailing slash) and returns "owner/repo" lowercased.
func githubRepoRoot(u string) (string, bool) {
	pu, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
		return "", false
	}
	host := strings.ToLower(pu.Host)
	if host != "github.com" && host != "www.github.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(pu.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	switch strings.ToLower(parts[0]) {
	case "sponsors", "topics", "orgs", "features", "marketplace", "collections", "apps", "settings", "about":
		return "", false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return strings.ToLower(parts[0] + "/" + repo), true
}

// listParser carries the state of one ParseList walk: the source bytes, the
// current heading stack (index = heading level) and the results so far.
type listParser struct {
	src      []byte
	headings [7]string
	items    []ParsedItem
	skips    SkipStats
}

// ParseList walks the goldmark AST of one awesome list README and extracts
// its items following importer step 3 of the build spec: the first link of
// each list item is the item, the text after it is the description, and the
// H2..H4 heading stack becomes the heading path. TOC sections, skipped
// headings and non-http links are counted in SkipStats instead.
func ParseList(md []byte) ([]ParsedItem, SkipStats) {
	p := &listParser{src: md, skips: SkipStats{}}
	p.walkBlocks(parseMarkdown(md))
	return p.items, p.skips
}

// walkBlocks visits block-level children in document order, tracking headings
// and treating every list not nested inside another list as a section.
func (p *listParser) walkBlocks(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c.Kind() {
		case ast.KindHeading:
			h := c.(*ast.Heading)
			p.headings[h.Level] = collapseSpace(plainText(h, p.src))
			for l := h.Level + 1; l < len(p.headings); l++ {
				p.headings[l] = ""
			}
		case ast.KindList:
			p.section(c)
		case ast.KindBlockquote:
			p.walkBlocks(c)
		}
	}
}

// underSkippedHeading reports whether any active heading (any level) is one
// of the non-content headings such as Contents or Contributing.
func (p *listParser) underSkippedHeading() bool {
	for _, h := range p.headings {
		if h != "" && isSkippedListHeading(h) {
			return true
		}
	}
	return false
}

// isSkippedListHeading matches the non-content headings exactly after
// normalization, plus "Related ..." variants such as "Related lists".
func isSkippedListHeading(h string) bool {
	n := normalizeHeading(h)
	return listSkipHeadings[n] || strings.HasPrefix(n, "related ")
}

// headingPath joins the active H2..H4 headings with " > ".
func (p *listParser) headingPath() string {
	var parts []string
	for l := 2; l <= 4; l++ {
		if p.headings[l] != "" {
			parts = append(parts, p.headings[l])
		}
	}
	return strings.Join(parts, " > ")
}

// section handles one top-level list: skipped wholesale when it sits under a
// non-content heading or when more than half of its links are in-page
// anchors (a table of contents), otherwise parsed item by item.
func (p *listParser) section(list ast.Node) {
	reason := ""
	if p.underSkippedHeading() {
		reason = "skipped_heading"
	} else if total, anchors := countLinks(list); total > 0 && anchors*2 > total {
		reason = "anchor_section"
	}
	if reason != "" {
		p.skips[reason] += countItems(list)
		return
	}
	p.list(list, p.headingPath())
}

// list parses every item of a list, recursing into nested sub-lists so each
// nested list item becomes its own item.
func (p *listParser) list(list ast.Node, path string) {
	for li := list.FirstChild(); li != nil; li = li.NextSibling() {
		p.item(li, path)
		for c := li.FirstChild(); c != nil; c = c.NextSibling() {
			if c.Kind() == ast.KindList {
				p.list(c, path)
			}
		}
	}
}

// item converts one list item into a ParsedItem or records why it was skipped.
func (p *listParser) item(li ast.Node, path string) {
	ex := extractItem(li, p.src)
	if reason := ex.skipReason(); reason != "" {
		p.skips[reason]++
		return
	}
	title := truncateWords(ex.title, maxTitleLen)
	if title == "" {
		p.skips["no_link"]++
		return
	}
	p.items = append(p.items, ParsedItem{
		Title:       title,
		URL:         ex.dest,
		Description: cleanDescription(ex.after.String()),
		HeadingPath: path,
	})
}

// itemExtract is the raw data pulled out of one list item: its first link
// (destination, plain-text title, whether it only wraps images) and the
// plain text that follows the link.
type itemExtract struct {
	link      ast.Node
	dest      string
	title     string
	imageOnly bool
	badgeImg  bool
	sawImage  bool // an image appeared before any link
	after     strings.Builder
}

// extractItem walks a list item's own content (nested lists excluded) in
// document order, capturing the first link and all text after it.
func extractItem(li ast.Node, src []byte) *itemExtract {
	ex := &itemExtract{}
	for c := li.FirstChild(); c != nil; c = c.NextSibling() {
		if c.Kind() == ast.KindList {
			continue
		}
		ex.visit(c, src)
		if ex.link != nil {
			ex.after.WriteByte(' ')
		}
	}
	return ex
}

// visit handles one node: before the first link it looks for that link (or
// a bare image); after it, it appends the node's plain text to the description.
func (ex *itemExtract) visit(n ast.Node, src []byte) {
	if ex.link != nil {
		writePlain(&ex.after, n, src)
		return
	}
	switch v := n.(type) {
	case *ast.Link:
		ex.link = v
		ex.dest = strings.TrimSpace(string(v.Destination))
		ex.title = collapseSpace(plainText(v, src))
		hasImg, badge := imagesIn(v)
		ex.imageOnly = hasImg && ex.title == ""
		ex.badgeImg = badge
		return
	case *ast.AutoLink:
		ex.link = v
		ex.dest = strings.TrimSpace(string(v.URL(src)))
		ex.title = collapseSpace(string(v.Label(src)))
		return
	case *ast.Image:
		ex.sawImage = true
		return
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		ex.visit(c, src)
	}
}

// skipReason classifies the extracted first link; "" means it is a valid item.
func (ex *itemExtract) skipReason() string {
	if ex.link == nil {
		if ex.sawImage {
			return "image"
		}
		return "no_link"
	}
	d := ex.dest
	if strings.HasPrefix(d, "#") {
		return "anchor"
	}
	if strings.HasPrefix(strings.ToLower(d), "mailto:") {
		return "mailto"
	}
	u, err := url.Parse(d)
	if err != nil {
		return "bad_scheme"
	}
	switch strings.ToLower(u.Scheme) {
	case "":
		return "relative"
	case "http", "https":
	default:
		return "bad_scheme"
	}
	if u.Host == "" {
		return "bad_scheme"
	}
	if isBadgeURL(d) || (ex.imageOnly && ex.badgeImg) {
		return "badge"
	}
	if ex.imageOnly {
		return "image"
	}
	return ""
}

// imagesIn reports whether a link contains images and whether any of them is
// a status badge.
func imagesIn(n ast.Node) (has, badge bool) {
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := c.(*ast.Image); ok && entering {
			has = true
			if isBadgeImage(string(img.Destination)) {
				badge = true
			}
		}
		return ast.WalkContinue, nil
	})
	return has, badge
}

// isBadgeURL detects links to status-badge services (shields.io, Travis, ...).
func isBadgeURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return badgeHosts[strings.TrimPrefix(strings.ToLower(u.Host), "www.")]
}

// isBadgeImage detects badge images: a badge service host or an image path
// that is obviously a badge (".../badge.svg", ".../status.svg", "/badge/...").
func isBadgeImage(raw string) bool {
	if isBadgeURL(raw) {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	return strings.HasSuffix(path, "badge.svg") || strings.HasSuffix(path, "badge.png") ||
		strings.HasSuffix(path, "status.svg") || strings.HasSuffix(path, "status.png") ||
		strings.Contains(path, "/badge/") || strings.HasSuffix(path, "/badge")
}

// countLinks counts links in a list subtree and how many are in-page anchors.
func countLinks(n ast.Node) (total, anchors int) {
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := c.(type) {
		case *ast.Link:
			total++
			if strings.HasPrefix(strings.TrimSpace(string(v.Destination)), "#") {
				anchors++
			}
		case *ast.AutoLink:
			total++
		}
		return ast.WalkContinue, nil
	})
	return total, anchors
}

// countItems counts list items in a subtree, nested ones included.
func countItems(n ast.Node) int {
	count := 0
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && c.Kind() == ast.KindListItem {
			count++
		}
		return ast.WalkContinue, nil
	})
	return count
}

// plainText renders a node's children as plain text (images and raw HTML
// dropped, escapes and entities resolved).
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		writePlain(&b, c, src)
	}
	return b.String()
}

// writePlain appends the plain-text rendering of n to b.
func writePlain(b *strings.Builder, n ast.Node, src []byte) {
	switch v := n.(type) {
	case *ast.Text:
		seg := v.Segment.Value(src)
		if !v.IsRaw() {
			seg = util.UnescapePunctuations(seg)
			seg = util.ResolveNumericReferences(seg)
			seg = util.ResolveEntityNames(seg)
		}
		b.Write(seg)
		if v.SoftLineBreak() || v.HardLineBreak() {
			b.WriteByte(' ')
		}
		return
	case *ast.String:
		b.Write(v.Value)
		return
	case *ast.AutoLink:
		b.Write(v.Label(src))
		return
	case *ast.Image, *ast.RawHTML, *ast.HTMLBlock:
		return
	case *ast.CodeSpan:
		for c := v.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				b.Write(t.Segment.Value(src))
			} else {
				writePlain(b, c, src)
			}
		}
		return
	}
	if n.Type() == ast.TypeBlock && b.Len() > 0 {
		b.WriteByte(' ')
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		writePlain(b, c, src)
	}
}

// cleanDescription turns the text after an item's link into a description:
// whitespace collapsed, one leading separator (-, –, —, :) removed, and the
// result truncated to 280 characters at a word boundary.
func cleanDescription(s string) string {
	s = collapseSpace(s)
	for _, sep := range []string{"-", "–", "—", ":"} {
		if strings.HasPrefix(s, sep) {
			s = strings.TrimSpace(strings.TrimPrefix(s, sep))
			break
		}
	}
	return truncateWords(s, maxDescriptionLen)
}

// collapseSpace trims s and collapses runs of whitespace to single spaces.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateWords shortens s to at most limit runes, cutting at the last word
// boundary and appending "…" when anything was removed.
func truncateWords(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)[:limit-1]
	cut := len(r)
	for i := len(r) - 1; i > 0; i-- {
		if unicode.IsSpace(r[i]) {
			cut = i
			break
		}
	}
	if cut < len(r)/2 { // one giant word: hard cut instead of dropping most of it
		cut = len(r)
	}
	out := strings.TrimRightFunc(string(r[:cut]), func(c rune) bool {
		return unicode.IsSpace(c) || unicode.IsPunct(c) && c != ')' && c != '"'
	})
	return out + "…"
}

// normalizeHeading lowercases a heading and drops emoji and punctuation so
// "📖 Table of Contents:" compares equal to "table of contents".
func normalizeHeading(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		}
	}
	return collapseSpace(b.String())
}
