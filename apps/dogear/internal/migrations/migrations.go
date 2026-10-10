// Package migrations embeds the goose SQL migrations and applies them.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed *.sql
var files embed.FS

// Up applies every pending migration. It holds a Postgres advisory lock while
// running so two processes started at once do not race each other.
func Up(ctx context.Context, databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(727274)"); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(727274)") //nolint:errcheck

	goose.SetBaseFS(files)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
