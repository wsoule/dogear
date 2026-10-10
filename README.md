# Dogear

A public directory of things people think are cool — sites, tools, apps, books,
devices, and videos — each with a one-line reason. It starts out pre-filled from
every list in [sindresorhus/awesome](https://github.com/sindresorhus/awesome)
(~670 lists, ~100k links), ranks them with a Wilson score seeded by GitHub
stars, and lets anyone keep their own lists with personal notes.

- **Minimal, server-rendered UI**: monospace, light and dark, htmx for the small
  interactive bits, works without JavaScript.
- **Lists**: save anything to your own public, unlisted, or private lists with a
  note per entry. Lists can sync to a GitHub repo README, and two ways with
  [loggit](https://useloggit.app) lists.
- **Agents welcome**: a REST API with an OpenAPI spec, an MCP server for Claude,
  a trimmed spec for GPT Actions, a browser extension, and a Claude Desktop
  bundle — all acting through per-user tokens with the same rules as the
  website.
- **Built to resist spam**: trust levels, rate limits, a classifier hook, Safe
  Browsing, link checking, SSRF-safe fetching, reports, and a mod queue.

## Repository layout

| Path                                                     | What                                                                                                                              |
| -------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| [`apps/dogear`](apps/dogear)                             | The Go app: one binary (`serve`, `migrate`, `import`, `linkcheck`) plus Postgres. Start with its [README](apps/dogear/README.md). |
| [`apps/dogear/extension`](apps/dogear/extension)         | Chrome/Firefox extension (plain TypeScript).                                                                                      |
| [`apps/dogear/claude-bundle`](apps/dogear/claude-bundle) | Claude Desktop extension (`.mcpb`).                                                                                               |
| [`docs/build-spec.md`](docs/build-spec.md)               | The original product and engineering spec.                                                                                        |
| `moon.yml`, `.moon/`, `packages/`                        | Monorepo tooling ([moon](https://moonrepo.dev), pnpm, oxlint/oxfmt).                                                              |

## Quick start

```bash
cd apps/dogear
cp .env.example .env          # set SESSION_SECRET: openssl rand -base64 32
createdb dogear               # Postgres 16 with citext + pg_trgm
make migrate-up
GITHUB_TOKEN=<token with no scopes> make import
make dev                      # http://localhost:8080
```

Repo-wide checks (TypeScript, formatting, lint) run through moon:

```bash
proto use && pnpm install
moon run root:format root:lint
```

## Contributing

Issues and pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md)
and the [code of conduct](CODE_OF_CONDUCT.md). Please report security issues
privately as described in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Imported list entries link back to their source lists;
descriptions are only shown from permissively licensed lists.
