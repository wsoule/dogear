package migrations

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestPersonalItemsMigration checks that 00016 accepts 'personal' items and
// that its down migration turns them into removed items instead of failing
// on the restored status check. It creates and drops its own database with
// scratchDB.
func TestPersonalItemsMigration(t *testing.T) {
	ctx := context.Background()
	db := scratchDB(t)
	if err := goose.UpToContext(ctx, db, ".", 16); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, username) VALUES (1, 'a')`)
	exec(`INSERT INTO items (id, url, url_norm, domain, title, status, created_by)
	  VALUES (10, 'https://x/10', 'x/10', 'x', 't', 'personal', 1), (11, 'https://x/11', 'x/11', 'x', 't', 'live', 1)`)
	exec(`INSERT INTO saves (user_id, item_id) VALUES (1, 10)`)

	if err := goose.DownToContext(ctx, db, ".", 15); err != nil {
		t.Fatalf("down to 15: %v", err)
	}
	var personal, live string
	var saves int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT status FROM items WHERE id = 10),
	  (SELECT status FROM items WHERE id = 11), (SELECT count(*) FROM saves)`).Scan(&personal, &live, &saves); err != nil {
		t.Fatal(err)
	}
	if personal != "removed" || live != "live" || saves != 1 {
		t.Fatalf("after down: personal item %q, live item %q, %d saves", personal, live, saves)
	}
	if _, err := db.ExecContext(ctx, `UPDATE items SET status = 'personal' WHERE id = 11`); err == nil {
		t.Fatal("status check still accepts 'personal' after down")
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
