package web

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dogear/internal/classify"
	"dogear/internal/config"
	"dogear/internal/db"
	"dogear/internal/migrations"
	"dogear/internal/service"
)

func TestShareURL(t *testing.T) {
	cases := []struct{ url, text, title, want string }{
		{"https://example.com/a", "", "", "https://example.com/a"},
		{"", "great read https://example.com/a?b=1 via @x", "", "https://example.com/a?b=1"},
		{"", "look (https://example.com/a).", "", "https://example.com/a"},
		{"", "“https://example.com/a”", "", "https://example.com/a"},
		{"not a link", "http://example.org/x", "", "http://example.org/x"},
		{"", "", "Title https://example.net/t", "https://example.net/t"},
		{"", "no links here", "just words", ""},
		{"javascript:alert(1)", "ftp://example.com/f", "", ""},
	}
	for _, c := range cases {
		if got := shareURL(c.url, c.text, c.title); got != c.want {
			t.Errorf("shareURL(%q, %q, %q) = %q, want %q", c.url, c.text, c.title, got, c.want)
		}
	}
}

func TestRenderOG(t *testing.T) {
	b, err := renderOG(ogCard{Title: "A very long tag title that will certainly need to wrap onto a second line 🎉",
		Byline: "by @someone · 12 things", Items: []string{"One", "两个 two", strings.Repeat("long ", 60), "Four"}, More: 9, Footer: "dogear.fyi/u/someone/tag"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatal("not a PNG")
	}
	if out := os.Getenv("OG_SAMPLE"); out != "" {
		_ = os.WriteFile(out, b, 0o644)
	}
}

// shareFixture is a server over a migrated test database with one user who
// has a public, an unlisted, and a private tag.
type shareFixture struct {
	srv     http.Handler
	svc     *service.Service
	owner   *service.Actor
	public  int64 // item only in the public tag
	private int64 // item in both the public and the private tag
	hidden  int64 // item only in the private tag
}

var fixtureSeq = time.Now().UnixNano() % 1_000_000

func newShareFixture(t *testing.T) shareFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrations.Up(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := &config.Config{Env: "development", BaseURL: "http://test", Signups: "open", IPHashSalt: "x",
		SessionSecret: bytes.Repeat([]byte("k"), 32)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := service.New(pool, cfg, classify.Noop{}, log)
	site, err := New(Options{Service: svc, Config: cfg, Log: log})
	if err != nil {
		t.Fatal(err)
	}

	fixtureSeq++
	name := fmt.Sprintf("sh%07d", fixtureSeq)
	u, err := svc.SignUp(ctx, service.ExternalIdentity{Provider: "github", ProviderUserID: name}, name, "", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetTrust(ctx, u.ID, 1); err != nil {
		t.Fatal(err)
	}
	u.TrustLevel = 1
	a := &service.Actor{User: u, Via: "web"}
	item := func(title string) int64 {
		fixtureSeq++
		link := fmt.Sprintf("https://share%d.test/page", fixtureSeq)
		it, err := svc.Q.InsertItem(ctx, db.InsertItemParams{Url: link, UrlNorm: link, Domain: "share.test",
			Title: title, Blurb: "A blurb for " + title, CreatedBy: &u.ID, Via: "web", Status: "live", Kind: "site", Flags: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	f := shareFixture{srv: site.Handler(), svc: svc, owner: a,
		public: item("Public thing"), private: item("Also private thing"), hidden: item("Secret thing")}
	for _, l := range []service.ListInput{{Title: "Open", Visibility: "public"}, {Title: "Link only", Visibility: "unlisted"}, {Title: "Mine", Visibility: "private"}} {
		if _, err := svc.CreateList(ctx, a, l); err != nil {
			t.Fatal(err)
		}
	}
	tag := func(slug string, id int64, note string) {
		if err := svc.SaveToList(ctx, a, slug, id, note, nil); err != nil {
			t.Fatal(err)
		}
	}
	tag("open", f.public, "my note on it")
	tag("open", f.private, "")
	tag("link-only", f.hidden, "")
	tag("mine", f.private, "")
	tag("mine", f.hidden, "")
	return f
}

func (f shareFixture) get(t *testing.T, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func TestOGVisibility(t *testing.T) {
	f := newShareFixture(t)
	u := f.owner.User.Username
	for path, want := range map[string]int{
		"/og/u/" + u + ".png":               200,
		"/og/u/" + u + "/open.png":          200,
		"/og/u/" + u + "/link-only.png":     404,
		"/og/u/" + u + "/mine.png":          404,
		"/og/u/" + u + "/missing.png":       404,
		"/og/u/" + u + "/open":              404,
		"/og/u/nobody-here-at-all.png":      404,
		"/og/u/" + u + "/open.png?cache=no": 200,
	} {
		rec := f.get(t, path)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
			continue
		}
		if want == 200 {
			if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
				t.Errorf("GET %s content type %q", path, ct)
			}
			if !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=3600") {
				t.Errorf("GET %s cache control %q", path, rec.Header().Get("Cache-Control"))
			}
			if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG")) {
				t.Errorf("GET %s did not return a PNG", path)
			}
		}
	}
	// The ETag revalidates without re-sending the image.
	rec := f.get(t, "/og/u/"+u+"/open.png")
	req := httptest.NewRequest("GET", "/og/u/"+u+"/open.png", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	f.srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("revalidate = %d, want 304", rec2.Code)
	}
}

// parsedRSS is the subset of RSS 2.0 the tests check.
type parsedRSS struct {
	Channel struct {
		Title string `xml:"title"`
		// No channel Link: encoding/xml matches <atom:link> to it too.
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			GUID        string `xml:"guid"`
			PubDate     string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

func TestRSS(t *testing.T) {
	f := newShareFixture(t)
	u := f.owner.User.Username

	rec := f.get(t, "/u/"+u+"/feed.xml")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/rss+xml") {
		t.Fatalf("archive feed = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom">`) || !strings.Contains(body, `<atom:link href="http://test/u/`+u+`/feed.xml" rel="self"`) {
		t.Errorf("archive feed head:\n%s", body)
	}
	var doc parsedRSS
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	// Saves in a private tag never reach the public archive feed.
	if len(doc.Channel.Items) != 1 {
		t.Fatalf("archive feed has %d items, want 1:\n%s", len(doc.Channel.Items), body)
	}
	it := doc.Channel.Items[0]
	if it.Title != "Public thing" || !strings.HasPrefix(it.Link, "https://share") || it.Description != "my note on it" ||
		it.GUID != fmt.Sprintf("http://test/i/%d", f.public) {
		t.Errorf("archive item = %+v", it)
	}
	if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
		t.Errorf("pubDate %q: %v", it.PubDate, err)
	}

	rec = f.get(t, "/u/"+u+"/open/feed.xml")
	if rec.Code != 200 {
		t.Fatalf("tag feed = %d", rec.Code)
	}
	doc = parsedRSS{}
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "<link>http://test/u/"+u+"/open</link>") || len(doc.Channel.Items) != 2 {
		t.Fatalf("tag feed = %+v", doc.Channel)
	}
	// Without a note, the description falls back to the item's blurb.
	if d := doc.Channel.Items[0].Description; d != "A blurb for Also private thing" {
		t.Errorf("newest tag item description = %q", d)
	}

	for _, slug := range []string{"link-only", "mine"} {
		if rec := f.get(t, "/u/"+u+"/"+slug+"/feed.xml"); rec.Code != 404 {
			t.Errorf("%s feed = %d, want 404", slug, rec.Code)
		}
	}
}

func TestEmbedFraming(t *testing.T) {
	f := newShareFixture(t)
	u := f.owner.User.Username

	rec := f.get(t, "/u/"+u+"/open/embed")
	if rec.Code != 200 {
		t.Fatalf("embed = %d", rec.Code)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "" {
		t.Errorf("embed X-Frame-Options = %q, want none", xfo)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors *") || strings.Contains(csp, "script-src") {
		t.Errorf("embed CSP = %q", csp)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Public thing") || strings.Contains(body, `class="wrap"`) || !strings.Contains(body, "on dogear") {
		t.Errorf("embed body:\n%s", body)
	}

	// Every other page, including the tag page itself, still refuses framing.
	rec = f.get(t, "/u/"+u+"/open")
	if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("tag page framing headers = %q / %q", rec.Header().Get("X-Frame-Options"), rec.Header().Get("Content-Security-Policy"))
	}
	for _, slug := range []string{"link-only", "mine"} {
		if rec := f.get(t, "/u/"+u+"/"+slug+"/embed"); rec.Code != 404 {
			t.Errorf("%s embed = %d, want 404", slug, rec.Code)
		}
	}
}

func TestSharePagesMeta(t *testing.T) {
	f := newShareFixture(t)
	u := f.owner.User.Username

	body := f.get(t, "/u/"+u+"/open").Body.String()
	for _, want := range []string{
		`<meta property="og:image" content="http://test/og/u/` + u + `/open.png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<link rel="alternate" type="application/rss+xml" title="Open · @`+u+`" href="http://test/u/` + u + `/open/feed.xml">`,
		`/u/` + u + `/open/embed`,
		`<a href="/signin">Make your own dogear`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tag page lacks %s", want)
		}
	}
	body = f.get(t, "/u/"+u).Body.String()
	if !strings.Contains(body, `content="http://test/og/u/`+u+`.png"`) || !strings.Contains(body, `<a href="/signin">Make your own dogear`) {
		t.Errorf("archive page lacks og:image or call to action")
	}
	// Unlisted tags get no preview card, feed, or embed snippet.
	body = f.get(t, "/u/"+u+"/link-only").Body.String()
	if strings.Contains(body, "og:image") || strings.Contains(body, "/embed") {
		t.Errorf("unlisted tag page advertises share surfaces")
	}
	// Signed in, the call to action is gone.
	raw, err := f.svc.CreateSession(context.Background(), f.owner.ID())
	if err != nil {
		t.Fatal(err)
	}
	body = f.get(t, "/u/"+u+"/open", &http.Cookie{Name: sessionCookie, Value: raw}).Body.String()
	if strings.Contains(body, "Make your own dogear") {
		t.Errorf("signed-in tag page shows the call to action")
	}
}

func TestShareTarget(t *testing.T) {
	f := newShareFixture(t)

	rec := f.get(t, "/share?text=see+https://new.example/x+now")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/signin?next="+"%2Fshare%3Ftext%3Dsee%2Bhttps%3A%2F%2Fnew.example%2Fx%2Bnow" {
		t.Errorf("signed out: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	raw, err := f.svc.CreateSession(context.Background(), f.owner.ID())
	if err != nil {
		t.Fatal(err)
	}
	sid := &http.Cookie{Name: sessionCookie, Value: raw}
	rec = f.get(t, "/share?text=see+https://new.example/x+now&title=New", sid)
	if loc := rec.Header().Get("Location"); loc != "/submit?title=New&url=https%3A%2F%2Fnew.example%2Fx" {
		t.Errorf("new link -> %q", loc)
	}
	it, err := f.svc.Q.GetItem(context.Background(), f.public)
	if err != nil {
		t.Fatal(err)
	}
	rec = f.get(t, "/share?url="+it.Url, sid)
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/i/%d", f.public) {
		t.Errorf("existing link -> %q", loc)
	}
	rec = f.get(t, "/share?text=nothing", sid)
	if loc := rec.Header().Get("Location"); loc != "/submit" {
		t.Errorf("no link -> %q", loc)
	}
}

// TestSigninNext checks that the sign-in page carries a same-site return
// path to the provider links (the share target relies on it) and drops an
// off-site one.
func TestSigninNext(t *testing.T) {
	ts, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for next, want := range map[string]string{
		"/share?url=https://x.example/a": `href="/auth/github?next=%2fshare%3furl%3dhttps%3a%2f%2fx.example%2fa"`,
		"//evil.example":                 `href="/auth/github"`,
	} {
		var b strings.Builder
		p := &Page{Data: signinPage{Providers: []string{"github"}, Next: returnPath(next)}}
		if err := ts["signin"].ExecuteTemplate(&b, "layout", p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("next %q: want %s in\n%s", next, want, b.String())
		}
	}
}
