# TypeScript Monorepo Template

Starter template for new projects, derived from the
[Pierre](https://github.com/pierredotco) open-source monorepo. It keeps the
toolchain — pnpm workspaces with a catalog, [moon](https://moonrepo.dev) as task
runner, [proto](https://moonrepo.dev/docs/proto) for tool version pins,
oxlint/oxfmt/stylelint, Bun's test runner, CI, and agent instructions — without
any product source code.

## Start

```bash
proto use        # install pinned bun, pnpm, node, moon
pnpm install
moon run root:format root:lint
moonx template:build
moonx template:test
moonx template:typecheck
```

## Layout

- `packages/template` — placeholder library wired into the full pipeline (tsdown
  build, bun test, tsgo typecheck). Rename or copy it to start real work; new
  libraries go under `packages/`, new apps under `apps/`.
- `moon.yml` — root project (repo-wide lint/format/clean/worktree tasks). Keep
  its `dependsOn` list in sync with dist-producing packages.
- `.moon/tasks/` — shared task definitions inherited by every project.
- `.agents/skills/` — agent-facing conventions; `AGENTS.md` is the entry point.
- `scripts/` — worktree manager (`wt.ts`), dev-server helpers; see
  `scripts/README.md`.
