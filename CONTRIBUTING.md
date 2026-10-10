# Contributing to Dogear

Thanks for helping. Small, focused pull requests are easiest to review.

## Before you start

- For anything bigger than a bug fix, open an issue first so we can agree on the
  approach.
- Read [`AGENTS.md`](AGENTS.md) (repo conventions, also used by coding agents)
  and [`apps/dogear/README.md`](apps/dogear/README.md) (how the app is put
  together).

## Development

```bash
cd apps/dogear
cp .env.example .env
createdb dogear && make migrate-up
make dev
```

- Business rules live in `internal/service`; handlers only parse input and
  render output. The web UI, REST API, and MCP server all go through it.
- SQL lives in `internal/db/queries/*.sql`; regenerate with `make sqlc`.
- Schema changes are new goose migrations in `internal/migrations`; never edit a
  migration that has shipped.
- Keep the UI minimal: no new JavaScript frameworks, no third-party scripts,
  styles, or images (the CSP only allows `'self'`).

## Checks

Run these before opening a pull request:

```bash
cd apps/dogear
gofmt -l .                       # must print nothing
go vet ./...
go test ./...
createdb dogear_test             # once
TEST_DATABASE_URL=postgres:///dogear_test go test ./internal/service/
cd ../.. && moon run root:format root:lint
```

CI runs the same checks.

## Pull requests

- Describe what changed and why; link the issue.
- Add or update tests for behavior changes.
- Mention any migration, new environment variable, or new outbound request.
- By contributing you agree your work is released under the
  [MIT License](LICENSE).
