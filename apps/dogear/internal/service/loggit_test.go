package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dogear/internal/db"
	"dogear/internal/loggit"
)

// fakeLoggit is an in-memory loggit lists API implementing the parts of the
// contract the sync uses.
type fakeLoggit struct {
	mu      sync.Mutex
	lists   map[string]*loggit.List
	media   map[string]loggit.MediaRef // url -> media
	nextID  int
	clock   time.Time
	byMedia map[string]loggit.MediaRef // id -> media
}

func newFakeLoggit() *fakeLoggit {
	f := &fakeLoggit{lists: map[string]*loggit.List{}, media: map[string]loggit.MediaRef{}, byMedia: map[string]loggit.MediaRef{},
		clock: time.Now().Add(time.Hour)}
	for _, m := range []loggit.MediaRef{
		{ID: "m-movie", Type: "movie", Title: "Arrival", URL: "https://www.themoviedb.org/movie/329865"},
		{ID: "m-book", Type: "book", Title: "Dune", URL: "https://openlibrary.org/works/OL893415W"},
		{ID: "m-game", Type: "boardgame", Title: "Root", URL: "https://boardgamegeek.com/boardgame/237182"},
	} {
		f.media[m.URL] = m
		f.byMedia[m.ID] = m
	}
	return f
}

func (f *fakeLoggit) tick() time.Time { f.clock = f.clock.Add(time.Second); return f.clock }

func (f *fakeLoggit) live(l *loggit.List) []loggit.Entry {
	var out []loggit.Entry
	for _, e := range l.Entries {
		if e.DeletedAt == nil {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b loggit.Entry) int { return a.Position - b.Position })
	return out
}

func (f *fakeLoggit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/media/resolve":
		m, ok := f.media[fmt.Sprint(body["url"])]
		if !ok {
			w.WriteHeader(404)
			return
		}
		write(map[string]any{"media": m})
	case r.Method == "POST" && r.URL.Path == "/lists":
		f.nextID++
		l := &loggit.List{ID: fmt.Sprintf("L%d", f.nextID), Title: fmt.Sprint(body["title"]), Visibility: "public", UpdatedAt: f.tick()}
		f.lists[l.ID] = l
		write(l)
	case len(parts) >= 2 && parts[0] == "lists":
		l, ok := f.lists[parts[1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		switch {
		case len(parts) == 2 && r.Method == "GET":
			out := *l
			out.Entries = f.live(l)
			write(out)
		case len(parts) == 2 && r.Method == "PATCH":
			if t, ok := body["title"].(string); ok {
				l.Title = t
			}
			l.UpdatedAt = f.tick()
			write(l)
		case len(parts) == 3 && parts[2] == "entries" && r.Method == "POST":
			mid := fmt.Sprint(body["mediaId"])
			for _, e := range f.live(l) {
				if e.MediaID == mid {
					w.WriteHeader(409)
					write(map[string]string{"error": "duplicate", "entryId": e.ID})
					return
				}
			}
			f.nextID++
			e := loggit.Entry{ID: fmt.Sprintf("E%d", f.nextID), ListID: l.ID, MediaID: mid, Note: fmt.Sprint(body["note"]),
				Position: len(f.live(l)), UpdatedAt: f.tick(), Media: f.byMedia[mid]}
			l.Entries = append(l.Entries, e)
			l.UpdatedAt = f.tick()
			w.WriteHeader(201)
			write(e)
		case len(parts) == 4 && parts[2] == "entries":
			for i := range l.Entries {
				if l.Entries[i].ID == parts[3] {
					if r.Method == "DELETE" {
						d := "x"
						l.Entries[i].DeletedAt = &d
					} else if n, ok := body["note"].(string); ok {
						l.Entries[i].Note = n
					}
					l.Entries[i].UpdatedAt = f.tick()
				}
			}
			l.UpdatedAt = f.tick()
			w.WriteHeader(204)
		case len(parts) == 3 && parts[2] == "order":
			ids, _ := body["entryIds"].([]any)
			for pos, id := range ids {
				for i := range l.Entries {
					if l.Entries[i].ID == id {
						l.Entries[i].Position = pos
					}
				}
			}
			l.UpdatedAt = f.tick()
			w.WriteHeader(204)
		}
	default:
		w.WriteHeader(404)
	}
}

// remoteSet lists a fake list's live media ids in order.
func (f *fakeLoggit) remoteSet(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.live(f.lists[id]) {
		out = append(out, e.MediaID)
	}
	return out
}

func TestLoggitTwoWaySync(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	fake := newFakeLoggit()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s.Loggit = &loggit.Client{APIURL: srv.URL, WebURL: srv.URL, ClientID: "c", ClientSecret: "s", HTTP: srv.Client()}
	s.Cfg.SessionSecret = []byte("0123456789abcdef0123456789abcdef")

	owner := newUser(t, s, 1)
	if err := s.LoggitConnect(ctx, owner.ID(), loggit.Token{AccessToken: "lga_x", RefreshToken: "lgr_x", ExpiresIn: 3600,
		User: loggit.User{ID: "u1", Username: "owner"}}); err != nil {
		t.Fatal(err)
	}
	// Dogear list: a movie (syncable) and a GitHub link (Dogear-only).
	movie := itemWithURL(t, s, owner, "https://www.themoviedb.org/movie/329865")
	link := itemWithURL(t, s, owner, fmt.Sprintf("https://github.com/x/tool%d", seq))
	l, err := s.CreateList(ctx, owner, ListInput{Title: "Weekend picks"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{movie, link} {
		if err := s.SaveToList(ctx, owner, l.Slug, id, "great", nil); err != nil {
			t.Fatal(err)
		}
	}
	// loggit list already has a book.
	fake.lists["R1"] = &loggit.List{ID: "R1", Title: "Weekend picks", Visibility: "public", UpdatedAt: fake.tick(),
		Entries: []loggit.Entry{{ID: "E-book", MediaID: "m-book", Note: "classic", UpdatedAt: fake.tick(), Media: fake.byMedia["m-book"]}}}

	if err := s.LinkList(ctx, owner, l.Slug, "R1"); err != nil {
		t.Fatal(err)
	}
	if got := fake.remoteSet("R1"); !slices.Equal(got, []string{"m-movie", "m-book"}) && !slices.Equal(got, []string{"m-book", "m-movie"}) {
		t.Fatalf("remote after link: %v", got)
	}
	lv, _ := s.GetList(ctx, owner, owner.User.Username, l.Slug)
	if len(lv.Entries) != 3 {
		t.Fatalf("local after link has %d entries, want 3 (movie, link, book)", len(lv.Entries))
	}

	sync := func() {
		t.Helper()
		link, err := s.Q.GetListLink(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SyncLink(ctx, link); err != nil {
			t.Fatal(err)
		}
	}
	// A note edited on Dogear reaches loggit.
	if err := s.SaveToList(ctx, owner, l.Slug, movie, "watch with headphones", nil); err != nil {
		t.Fatal(err)
	}
	sync()
	for _, e := range fake.lists["R1"].Entries {
		if e.MediaID == "m-movie" && e.Note != "watch with headphones" {
			t.Fatalf("remote note = %q", e.Note)
		}
	}
	// A delete on loggit removes the Dogear entry; the Dogear-only link stays.
	for i := range fake.lists["R1"].Entries {
		if fake.lists["R1"].Entries[i].MediaID == "m-book" {
			d := "x"
			fake.lists["R1"].Entries[i].DeletedAt = &d
		}
	}
	sync()
	lv, _ = s.GetList(ctx, owner, owner.User.Username, l.Slug)
	var ids []int64
	for _, e := range lv.Entries {
		ids = append(ids, e.Item.ID)
	}
	if len(ids) != 2 || !slices.Contains(ids, movie) || !slices.Contains(ids, link) {
		t.Fatalf("local after remote delete: %v", ids)
	}
	// Unchanged lists sync as a no-op.
	before := fake.tick()
	sync()
	if fake.lists["R1"].UpdatedAt.After(before) {
		t.Fatal("no-op sync wrote to loggit")
	}
}

func itemWithURL(t *testing.T, s *Service, by *Actor, u string) int64 {
	t.Helper()
	if it, err := s.Q.GetItemByNorm(context.Background(), u); err == nil {
		return it.ID // left over from an earlier run against the same database
	}
	seq++
	it, err := s.Q.InsertItem(context.Background(), db.InsertItemParams{Url: u, UrlNorm: u, Domain: "x.test",
		Title: fmt.Sprintf("Thing %d", seq), CreatedBy: &by.User.ID, Via: "web", Status: "live", Kind: "site", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}
