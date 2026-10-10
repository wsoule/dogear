# Dogear

A public directory of things people think are cool — sites, tools, apps, books,
devices, videos — each with a one-line reason. Seeded from every list in
[sindresorhus/awesome](https://github.com/sindresorhus/awesome), ranked by a
Wilson score, with personal lists, a REST API, and an MCP server so Claude and
GPT agents can post and save on a user's behalf.

One Go binary plus Postgres. No JavaScript framework; server-rendered HTML with
htmx and one hand-written stylesheet (monospace, one accent, light and dark,
with a single-line dog-ear mark in the top right).

## Run locally

Needs Go (see `go.mod`) and Postgres 16 with `citext` and `pg_trgm`.

```bash
cp .env.example .env          # fill SESSION_SECRET (openssl rand -base64 32)
createdb dogear               # or any Postgres 16; set DATABASE_URL
make migrate-up
GITHUB_TOKEN=<no-scope PAT> make import   # ~670 lists, ~100k items
make dev                      # http://localhost:8080
```

Sign-in needs at least one OAuth provider in `.env` (GitHub is easiest: callback
`http://localhost:8080/auth/github/callback`). Set `ADMIN_GITHUB_ID` to your
numeric GitHub id to become admin; admins skip the invite requirement.

Through moon from anywhere in the monorepo: `moonx dogear:build`,
`moonx dogear:test`, `moonx dogear:typecheck` (go vet).

## Commands

| Command                                                 | What it does                                                                 |
| ------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `dogear serve`                                          | Web UI, `/api/v1`, `/mcp`. Migrates on start unless `MIGRATE_ON_START=false` |
| `dogear migrate`                                        | Apply migrations and exit                                                    |
| `dogear import [--force] [--only owner/repo] [--stars]` | Import awesome lists; idempotent and resumable                               |
| `dogear linkcheck [--batch 2000]`                       | Check links; 3 consecutive hard failures mark an item dead                   |

## Layout

```
cmd/dogear/            subcommands
internal/config        env parsing, validated at startup
internal/migrations    goose SQL, embedded
internal/db            sqlc queries + generated code, hand-written feed query
internal/service       every business rule (web, API, and MCP all call it)
internal/ranking       Wilson score and priors
internal/importer      GitHub fetch, markdown parse, upsert
internal/auth          GitHub/Google/Apple OAuth, signed cookies, CSRF, Turnstile
internal/safety        URL normalizer, SSRF-safe client, rate limiter, sanitizer
internal/classify      Jev classifier, policy, page extraction, eval set
internal/ghsync        GitHub App + README renderer + sync worker
internal/linkcheck     nightly link checker
internal/web           handlers, templates, static (htmx, app.css)
internal/api           REST handlers, openapi.yaml, gpt.yaml
internal/mcp           MCP server (streamable HTTP, stateless)
extension/             Chrome/Firefox MV3 extension (plain TypeScript)
claude-bundle/         Claude Desktop .mcpb bundle (stdio server)
scripts/redteam/       abuse probe for a staging deployment
```

## Tests

```bash
go test ./...                                         # unit + golden tests
TEST_DATABASE_URL=postgres:///dogear_test go test ./internal/service/   # DB integration
JEV_API_KEY=... go test ./internal/classify -run Eval -v                 # classifier eval
go run ./scripts/redteam -base https://staging... -token afk_...         # staging only
```

Staging builds (`BUILD_TAGS=staging`, `ENV=staging`) add `/test/login` for the
red-team script. Production builds don't contain it.

## Deploy (Railway)

Services in project `dogear`: `web` (Dockerfile, health check `/healthz`),
`Postgres`, `importer` (cron `0 6 * * 1`, start `/app/dogear import`), and
`linkcheck` (cron `0 9 * * *`, start `/app/dogear linkcheck`). Variables are
listed in `.env.example`. Keep `web` at one replica: rate limits are in memory.

## API and agents

- REST: `/api/v1`, spec at `/api/v1/openapi.yaml`; GPT Actions use the trimmed
  `/api/v1/gpt.yaml`.
- MCP:
  `claude mcp add --transport http dogear https://dogear.fyi/mcp --header "Authorization: Bearer afk_..."`
- Tokens are created at `/settings/tokens`. A token can never create tokens or
  delete the account.
