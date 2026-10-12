package classify

import (
	"bytes"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxPageText is how many characters of visible text ExtractPage keeps.
const maxPageText = 2000

// Page is the metadata and visible text pulled from a fetched HTML page.
type Page struct {
	Title, Description, Text, Author, SiteName, OGTitle string
	// Image is the og:image or twitter:image URL as written on the page
	// (possibly relative); callers resolve it against the page URL.
	Image string
	// OGType is og:type ("article", "website", ...) and Published is
	// article:published_time as written on the page.
	OGType, Published string
	// Words counts every visible word, not just the kept Text, for reading
	// time.
	Words int
}

// hiddenElems are elements whose text content is never visible.
var hiddenElems = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true,
	atom.Svg: true, atom.Iframe: true, atom.Object: true,
}

// ExtractPage parses HTML (already size-capped by the caller) and returns the
// <title>, meta description (name=description, else og:description), the
// first 2000 characters of visible text with script/style/noscript/template
// removed and whitespace collapsed, creator hints (meta author, else
// article:author; og:site_name), and og:title. It never fails: malformed HTML
// yields whatever could be read.
func ExtractPage(doc []byte) Page {
	var (
		p                     Page
		metaDesc, ogDesc      string
		metaAuthor, artAuthor string
		text                  strings.Builder
		textLen               int
		hiddenDepth           int
		inTitle, haveTitle    bool
		title                 strings.Builder
		lastSpace             = true
	)
	// addText appends s with whitespace collapsed, stopping at maxPageText
	// runes, and counts its words.
	inWord := false
	addText := func(s string) {
		for _, r := range s {
			if unicode.IsSpace(r) {
				inWord = false
			} else if !inWord {
				inWord = true
				p.Words++
			}
		}
		for _, r := range s {
			if textLen >= maxPageText {
				return
			}
			if unicode.IsSpace(r) {
				if !lastSpace {
					text.WriteByte(' ')
					textLen++
					lastSpace = true
				}
				continue
			}
			text.WriteRune(r)
			textLen++
			lastSpace = false
		}
	}

	z := html.NewTokenizer(bytes.NewReader(doc))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			switch tok.DataAtom {
			case atom.Title:
				if tt == html.StartTagToken && !haveTitle {
					inTitle = true
				}
			case atom.Meta:
				name, prop, content := "", "", ""
				for _, a := range tok.Attr {
					switch strings.ToLower(a.Key) {
					case "name":
						name = strings.ToLower(strings.TrimSpace(a.Val))
					case "property":
						prop = strings.ToLower(strings.TrimSpace(a.Val))
					case "content":
						content = collapse(a.Val)
					}
				}
				setOnce := func(dst *string) {
					if *dst == "" {
						*dst = content
					}
				}
				switch {
				case name == "description":
					setOnce(&metaDesc)
				case name == "author":
					setOnce(&metaAuthor)
				case prop == "og:description" || name == "og:description":
					setOnce(&ogDesc)
				case prop == "og:site_name" || name == "og:site_name":
					setOnce(&p.SiteName)
				case prop == "og:title" || name == "og:title":
					setOnce(&p.OGTitle)
				case prop == "og:image" || prop == "og:image:url" || prop == "og:image:secure_url" ||
					name == "twitter:image" || name == "twitter:image:src" || prop == "twitter:image":
					setOnce(&p.Image)
				case prop == "article:author" || name == "article:author":
					setOnce(&artAuthor)
				case prop == "og:type" || name == "og:type":
					setOnce(&p.OGType)
				case prop == "article:published_time" || name == "article:published_time":
					setOnce(&p.Published)
				}
			case atom.Br, atom.P, atom.Div, atom.Li, atom.Td, atom.Th, atom.Tr,
				atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Section, atom.Article:
				addText(" ")
			}
			if tt == html.StartTagToken && hiddenElems[tok.DataAtom] {
				hiddenDepth++
			}
		case html.EndTagToken:
			if tok.DataAtom == atom.Title && inTitle {
				inTitle, haveTitle = false, true
			}
			if hiddenElems[tok.DataAtom] && hiddenDepth > 0 {
				hiddenDepth--
			}
			addText(" ")
		case html.TextToken:
			switch {
			case inTitle:
				title.WriteString(tok.Data)
			case hiddenDepth == 0:
				addText(tok.Data)
			}
		}
	}

	p.Title = collapse(title.String())
	p.Description = metaDesc
	if p.Description == "" {
		p.Description = ogDesc
	}
	p.Author = metaAuthor
	if p.Author == "" {
		p.Author = artAuthor
	}
	p.Text = strings.TrimSpace(text.String())
	return p
}

// collapse trims s and replaces each run of whitespace with one space.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
