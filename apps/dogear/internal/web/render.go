package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"dogear/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Page is the data every full page template receives.
type Page struct {
	Title, Description string
	NoIndex            bool
	Viewer             *service.Actor
	CSRF               string
	Nav                string
	Flash              string
	TurnstileSiteKey   string
	BaseURL            string
	ContactEmail       string
	Page               string // which static page to show
	Data               any
}

// rowData is what the "row" partial needs: the item, an optional list note,
// and the viewer's CSRF token for the vote form.
type rowData struct {
	Item   service.Item
	Note   string
	CSRF   string
	Viewer *service.Actor
}

type voteData struct {
	ItemID int64
	Likes  int
	MyVote int
	CSRF   string
}

// feedData drives the "feed" and "rows" partials including infinite scroll.
type feedData struct {
	Items    []service.Item
	Next     string // htmx URL for the next page fragment
	NextPage string // plain link for no-JS paging
	CSRF     string
	Viewer   *service.Actor
}

var funcs = template.FuncMap{
	"rowData": func(it service.Item, note, csrf string, v *service.Actor) rowData {
		return rowData{Item: it, Note: note, CSRF: csrf, Viewer: v}
	},
	"voteState": func(it service.Item, csrf string) voteData {
		v := voteData{ItemID: it.ID, Likes: it.Likes, CSRF: csrf}
		if it.MyVote != nil {
			v.MyVote = *it.MyVote
		}
		return v
	},
	"userActions": func(username, csrf string) map[string]string {
		return map[string]string{"Username": username, "CSRF": csrf}
	},
	"kinds":    func() []string { return service.Kinds },
	"list":     func(v ...string) []string { return v },
	"join":     strings.Join,
	"contains": strings.Contains,
	"date":     func(t time.Time) string { return t.Format("2006-01-02") },
	"deref": func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	},
	"derefTime": func(p *time.Time) time.Time {
		if p == nil {
			return time.Time{}
		}
		return *p
	},
}

// templates holds one parsed set per page file: the layout and partials
// plus that page's "content" block.
type templates map[string]*template.Template

func parseTemplates() (templates, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	// Partials used across pages are defined in page files too, so every set
	// also parses the files that define shared partials.
	shared := []string{"templates/item.html", "templates/search.html", "templates/submit.html", "templates/mod.html"}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := templates{}
	for _, p := range pages {
		if p == "templates/layout.html" {
			continue
		}
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		// Parse shared partial files first (their "content" is overwritten by
		// the page itself, which is parsed last).
		for _, sh := range shared {
			if sh != p {
				if _, err := t.ParseFS(templateFS, sh); err != nil {
					return nil, fmt.Errorf("%s: %w", sh, err)
				}
			}
		}
		if _, err := t.ParseFS(templateFS, p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		out[name] = t
	}
	return out, nil
}

// render writes a full page, or only the named fragment for htmx requests.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, tmpl string, p *Page) {
	s.fill(r, p)
	s.exec(w, status, tmpl, "layout", p)
}

// fragment renders one named partial (no layout), used for htmx swaps.
func (s *Server) fragment(w http.ResponseWriter, status int, tmpl, name string, data any) {
	s.exec(w, status, tmpl, name, data)
}

func (s *Server) exec(w http.ResponseWriter, status int, tmpl, name string, data any) {
	t, ok := s.tmpl[tmpl]
	if !ok {
		s.log.Error("missing template", "tmpl", tmpl)
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render", "tmpl", tmpl, "name", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// fill adds request-scoped fields every page needs.
func (s *Server) fill(r *http.Request, p *Page) {
	rc := reqCtx(r)
	p.Viewer = rc.actor
	p.CSRF = rc.csrf
	p.BaseURL = s.cfg.BaseURL
	p.ContactEmail = s.cfg.ContactEmail
	if p.Flash == "" {
		p.Flash = flashes[r.URL.Query().Get("m")]
	}
}

// flashes are the only status messages a redirect can show; using fixed
// codes means a crafted link cannot put arbitrary text on the page.
var flashes = map[string]string{
	"posted":     "Posted. It's live.",
	"pending":    "Thanks — your post is waiting for review and will appear once approved.",
	"saved":      "Saved.",
	"reported":   "Thanks, a moderator will take a look.",
	"welcome":    "Welcome to Dogear. Post something you think is cool, or save things to your Favorites.",
	"linked":     "Sign-in method linked.",
	"unlinked":   "Sign-in method unlinked.",
	"deleted":    "Your account was deleted.",
	"profile":    "Profile saved.",
	"revoked":    "Token revoked.",
	"connected":  "GitHub repo connected. The first sync runs within a few minutes.",
	"disconnect": "GitHub sync disconnected. Uninstall the app on GitHub to fully revoke access.",
	"done":       "Done.",
	"list":       "List created.",
	"listdel":    "List deleted.",
	"invite":     "Invite saved. Sign in to create your account.",
}
