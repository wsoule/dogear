package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// TestMigrationsRoundTrip applies every migration to an empty database,
// rolls back to version 12 (before saves, creators, topics, and personal
// items), and
// applies them again, so each down migration really undoes its up.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := scratchDB(t)
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, db, ".", 12); err != nil {
		t.Fatalf("down to 12: %v", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// scratchDB creates an empty database next to TEST_DATABASE_URL, set up
// for goose with this package's migrations, and drops it after the test.
// It skips the test when TEST_DATABASE_URL is not set.
func scratchDB(t *testing.T) *sql.DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := fmt.Sprintf("dogear_mig_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	goose.SetBaseFS(files)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	return db
}
