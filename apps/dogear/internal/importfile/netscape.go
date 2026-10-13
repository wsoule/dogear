package importfile

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// parseNetscape reads a Netscape bookmarks file: nested <DL> lists where
// each <DT><H3> names a folder and each <DT><A HREF ADD_DATE TAGS> is a
// bookmark, optionally followed by a <DD> description. Folder names (minus
// browser containers like "Bookmarks bar") and the TAGS attribute become
// tags. It never fails; malformed markup yields whatever could be read.
func parseNetscape(data []byte) []Entry {
	z := html.NewTokenizer(bytes.NewReader(data))
	var (
		out     []Entry
		folders []string // folder names, one per open <DL>
		pending string   // the last <H3> seen, waiting for its <DL>
		inH3    bool
		h3      strings.Builder
		cur     *Entry // the open <A>
		title   strings.Builder
		inDD    bool
		dd      strings.Builder
	)
	// endDD attaches a collected <DD> description to the last bookmark.
	endDD := func() {
		if inDD && len(out) > 0 && out[len(out)-1].Note == "" {
			out[len(out)-1].Note = strings.Join(strings.Fields(dd.String()), " ")
		}
		inDD = false
		dd.Reset()
	}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch atom.Lookup(name) {
			case atom.Dl:
				endDD()
				folders = append(folders, pending)
				pending = ""
			case atom.Dt:
				endDD()
			case atom.Dd:
				endDD()
				inDD = true
			case atom.H3:
				endDD()
				inH3 = true
				h3.Reset()
			case atom.A:
				endDD()
				e := Entry{}
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					switch strings.ToLower(string(k)) {
					case "href":
						e.URL = strings.TrimSpace(string(v))
					case "add_date":
						e.SavedAt = parseTime(string(v))
					case "tags":
						e.Tags = splitTags(string(v), ",")
					}
				}
				e.Tags = append(folderTags(folders), e.Tags...)
				cur = &e
				title.Reset()
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch atom.Lookup(name) {
			case atom.Dl:
				endDD()
				if len(folders) > 0 {
					folders = folders[:len(folders)-1]
				}
			case atom.H3:
				inH3 = false
				pending = strings.Join(strings.Fields(h3.String()), " ")
			case atom.A:
				if cur != nil {
					cur.Title = strings.Join(strings.Fields(title.String()), " ")
					if cur.URL != "" {
						out = append(out, *cur)
					}
					cur = nil
				}
			}
		case html.TextToken:
			switch {
			case cur != nil:
				title.Write(z.Text())
			case inH3:
				h3.Write(z.Text())
			case inDD:
				dd.Write(z.Text())
			}
		}
		if len(out) > MaxEntries {
			break
		}
	}
	endDD()
	return out
}
