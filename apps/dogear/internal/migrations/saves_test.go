package migrations

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestSavesMigration builds a database at version 12 holding lists, notes,
// and a Dogeared list, then checks that 00013 turns them into saves and that
// its down migration puts them back. It creates and drops its own database
// with scratchDB.
func TestSavesMigration(t *testing.T) {
	ctx := context.Background()
	db := scratchDB(t)
	if err := goose.UpToContext(ctx, db, ".", 12); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, username) VALUES (1, 'a'), (2, 'b'), (3, 'c')`)
	exec(`INSERT INTO items (id, url, url_norm, domain, title) VALUES
	  (10, 'https://x/10', 'x/10', 'x', 't'), (11, 'https://x/11', 'x/11', 'x', 't'), (12, 'https://x/12', 'x/12', 'x', 't'),
	  (13, 'https://x/13', 'x/13', 'x', 't')`)
	exec(`INSERT INTO lists (id, user_id, slug, title) VALUES
	  (100, 1, 'dogeared', 'Dogeared'), (101, 1, 'ai', 'AI'), (102, 1, 'essays', 'Essays'), (200, 2, 'dogeared', 'Dogeared')`)
	exec(`INSERT INTO lists (id, user_id, slug, title, visibility) VALUES
	  (103, 1, 'secret', 'Secret', 'private'), (300, 3, 'dogeared', 'Dogeared', 'private')`)
	// Item 10: dogeared, tagged twice with two notes, and noted on a
	// private list. Item 11: only on a tag. Item 12: dogeared by user 2,
	// synced to loggit. Item 13: in user 3's private Dogeared list.
	exec(`INSERT INTO list_items (list_id, item_id, note, added_at) VALUES
	  (100, 10, '',       '2026-01-01'),
	  (101, 10, 'primer', '2026-01-02'),
	  (102, 10, 'classic','2026-01-03'),
	  (101, 11, 'later',  '2026-01-04'),
	  (103, 10, 'hush',   '2026-01-02'),
	  (200, 12, 'mine',   '2026-01-05'),
	  (300, 13, '',       '2026-01-06')`)
	exec(`INSERT INTO list_links (list_id, user_id, loggit_list_id) VALUES (200, 2, 'L1')`)

	if err := goose.UpToContext(ctx, db, ".", 13); err != nil {
		t.Fatal(err)
	}
	type save struct {
		note string
		at   time.Time
	}
	got := map[[2]int64]save{}
	rows, err := db.QueryContext(ctx, `SELECT user_id, item_id, note, saved_at FROM saves`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k [2]int64
		var s save
		if err := rows.Scan(&k[0], &k[1], &s.note, &s.at); err != nil {
			t.Fatal(err)
		}
		got[k] = s
	}
	rows.Close()
	if len(got) != 4 {
		t.Fatalf("saves = %+v", got)
	}
	if s := got[[2]int64{1, 10}]; s.note != "primer · classic" || s.at.Day() != 1 {
		t.Errorf("item 10 save = %+v, want merged public notes (not the private one) and earliest date", s)
	}
	if s := got[[2]int64{1, 11}]; s.note != "later" {
		t.Errorf("item 11 note = %q", s.note)
	}
	var lists []string
	rows, err = db.QueryContext(ctx, `SELECT user_id || '/' || slug FROM lists ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		lists = append(lists, s)
	}
	rows.Close()
	if fmt.Sprint(lists) != "[1/ai 1/essays 1/secret 2/dogeared 3/dogeared]" {
		t.Errorf("lists after up = %v (public unlinked dogeared should be gone; linked or private kept)", lists)
	}

	if err := goose.DownToContext(ctx, db, ".", 12); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM list_items li JOIN lists l ON l.id = li.list_id
	  WHERE l.user_id = 1 AND l.slug = 'dogeared'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Item 10 and 11 are both tagged, so neither needs to come back to the
	// recreated Dogeared list.
	if n != 0 {
		t.Errorf("recreated dogeared list has %d entries, want 0", n)
	}
	var note string
	if err := db.QueryRowContext(ctx, `SELECT note FROM list_items WHERE list_id = 102 AND item_id = 10`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "primer · classic" {
		t.Errorf("note copied back = %q", note)
	}
	if err := db.QueryRowContext(ctx, `SELECT note FROM list_items WHERE list_id = 103 AND item_id = 10`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "hush" {
		t.Errorf("private list note after down = %q, want it kept", note)
	}
}
