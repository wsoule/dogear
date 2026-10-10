# scripts/

This folder contains the small set of scripts that drive day-to-day development
in the monorepo. Tasks (build/dev/test/lint) are run by moon —
`moon run <project>:<task>` / `moonx <project>:<task>` from anywhere in the repo
— and the scripts here cover what a task runner doesn't:

- `wt.ts` — manage `git worktree`s and their port isolation (exposed as the
  `root:wt` moon task).
- `run-dev.sh` — kill-before-start preamble for dev-server tasks.
- `chrome-remote-debug.sh` — launches Chrome Dev with the DevTools protocol
  enabled, worktree-aware (exposed as `root:chrome`).
- `load-worktree-env.mjs` — `.env.worktree` loader for configs that run outside
  a moon task (e.g. Next/Playwright configs).
- `assert-pnpm-version.ts` — publish guard behind the publishable packages'
  `prepublish` chain (asserts the pnpm version matches the `.prototools` pin).

The rest of this document explains `wt` in detail.

## `wt` — the worktree command suite

`wt` manages sibling worktrees of the main clone so you can work on multiple
branches in parallel without port collisions, zombie dev servers piling up, or
browser tabs you can't tell apart.

Every worktree managed by `wt` lives at:

```
~/worktrees/<repo>/<dir>/
```

where `<repo>` is the main clone's directory name and `<dir>` is the slug with
any `/` characters replaced by `-`. Worktrees the tool didn't create (e.g.
agent-spawned ones under `.omx/worktrees/` or `/private/tmp/`) are left alone.

### Subcommands at a glance

```bash
moonx root:wt -- new <slug> [--branch <name>] [--base <ref>]
moonx root:wt -- setup [<slug>]
moonx root:wt -- rm  <slug> [--keep-branch] [--force]
moonx root:wt -- clean [<slug>|--all]
moonx root:wt -- ps
moonx root:wt -- list
```

### `wt new <slug>` — create a worktree

Creates a new worktree rooted at `~/worktrees/<repo>/<slug>/`, on a fresh
branch, with ports that won't collide with any other worktree's.

By default:

- Branch name is `$USER/<slug>` (e.g. `alex/fix-drag-drop`).
- Base ref is `main`.
- A port offset is picked (see "Ports" below), deterministic from the slug but
  bumped if it collides with an existing worktree.
- `.env.worktree` is written at the worktree root with the offset and slug.
  Configs that need those keys outside of a moon task load the file themselves
  via `scripts/load-worktree-env.mjs` (or the inlined bash walk-up in
  `chrome-remote-debug.sh`).
- `pnpm install` runs automatically so the worktree's `node_modules` is ready;
  moon syncs its git hooks on the first moon command in the worktree.
- A summary of the worktree's ports and the `cd` command is printed.

Flags:

- `--branch <name>` — attach an **existing** branch to the new worktree, rather
  than creating a new one named `$USER/<slug>`.
- `--base <ref>` — when creating a fresh branch, root it at `<ref>` instead of
  `main`.

**Why `wt new` cannot `cd` for you.** A child process cannot change its parent
shell's working directory. The script prints the `cd` command at the end;
copy-paste it.

### `wt setup [<slug>]` — initialize an existing worktree

Runs the post-add setup that `wt new` runs automatically:

- Resolves the target worktree.
- Writes or reuses `.env.worktree` with `WORKTREE_SLUG` and
  `WORKTREE_PORT_OFFSET`.
- Runs `pnpm install` in the target worktree.
- Prints the worktree's ports and `cd` command.

Without a slug, it targets the current linked worktree — intended for scripts
that create or enter a worktree themselves before running setup. `wt setup`
refuses to initialize the main clone, because the main clone should not have a
`.env.worktree` and should keep offset 0.

### `wt rm <slug>` — tear down a worktree

Runs `wt clean <slug>` first (kills any processes bound to the worktree's
ports), then `git worktree remove`, then tries to delete the branch with
`git branch -d` (which refuses to delete unmerged branches — safe by default).

Flags:

- `--force` — passes `--force` through to `git worktree remove` (needed when the
  worktree has uncommitted changes).
- `--keep-branch` — skip the branch deletion step.

### `wt clean` — nuke zombies

Dev servers sometimes outlive the terminal that started them. When that happens,
subsequent starts either collide or silently launch a duplicate. `wt clean`
fixes this on demand:

- `moonx root:wt -- clean` — scans every managed worktree's expected ports and
  kills any process listening on them.
- `moonx root:wt -- clean <slug>` — same, but only for that worktree.

Agents in particular should run `moonx root:wt -- clean` before ending their
turn.

### `wt ps` / `wt list`

`ps` prints a table with one row per worktree and one column per registered
service (port number + PID if something is listening). `list` prints one line
per worktree showing slug, offset, branch, and path — including the main clone
(`(main)`, offset `—`) and unmanaged/agent worktrees (`(unmanaged)`).

---

## Ports: how the offset system works

Each port-bound dev/test service has a **base port**, registered in `PORT_BASES`
in `scripts/wt.ts`. The template starts with one:

| Service             | Base port |
| ------------------- | --------- |
| Chrome remote debug | 9222      |

Every worktree owns a **port offset** (0, 10, 20, 30, …). Its actual ports are
`base + offset`. Main clone is always offset 0 — its ports are unchanged.

`wt setup` picks an offset deterministically from the slug's hash (so recreating
a worktree with the same slug tends to give you the same ports). If that
candidate collides with another live worktree's offset, it bumps by 10 until it
finds a free slot. Discovery of live offsets is stateless: `git worktree list`
combined with each worktree's `.env.worktree` is the source of truth.

How the offset reaches dev tasks:

1. `wt setup` writes `.env.worktree` at the worktree root:
   ```
   WORKTREE_SLUG=drag-drop-fix
   WORKTREE_PORT_OFFSET=30
   ```
2. Port-binding moon tasks declare `envFile: '/.env.worktree'`, so moon loads
   those keys before the task's shell runs (the file is workspace-root relative
   and silently skipped in the main clone, which has none).
3. The task script uses shell arithmetic to derive the final port:
   `PORT=$(( ${WORKTREE_PORT_OFFSET:-0} + <your_base> ))`.

---

## `run-dev.sh` — kill-before-start

`scripts/run-dev.sh <PORT> -- <command> [args…]` is a tiny preamble for
dev-server tasks. It:

1. Runs `lsof -ti :$PORT -sTCP:LISTEN` to find any process currently bound to
   the port.
2. Sends SIGTERM, waits 300ms, sends SIGKILL to anything still bound.
3. Execs the downstream command.

This means **running a dev script always replaces the prior run on that port**.
Because the port offset guarantees no two live worktrees share a port,
`run-dev.sh` can never accidentally kill another worktree's server.

---

## `chrome-remote-debug.sh`

`moonx root:chrome` launches Chrome Dev with the DevTools remote-debugging
protocol on port 9222 (main clone) or `9222 + offset` (worktree). Each worktree
gets its own `--user-data-dir` (e.g. `/tmp/chrome-devtools-<slug>`) so Chromes
launched from different worktrees don't fight over a shared profile. After
launching, the script waits until the debug port actually accepts connections
before returning.

---

## Adding a new port-bound service

If you add a dev or E2E server with a fixed port, do three things:

1. Define the moon task with `envFile: '/.env.worktree'`, write the port as
   `PORT=$(( ${WORKTREE_PORT_OFFSET:-0} + <your_base> ))`, and wrap the command
   in `scripts/run-dev.sh`:

   ```yaml
   dev-myservice:
     script:
       'PORT=$(( ${WORKTREE_PORT_OFFSET:-0} + 4321 )) && bash
       ../../scripts/run-dev.sh "$PORT" -- my-server --port "$PORT"'
     options:
       cache: false
       persistent: true
       envFile: '/.env.worktree'
   ```

2. If a config file reads the port (e.g. Playwright configs), read from
   `process.env.WORKTREE_PORT_OFFSET` and add it to your base:

   ```ts
   const portOffset = Number(process.env.WORKTREE_PORT_OFFSET ?? 0);
   const e2ePort = 4321 + portOffset;
   ```

3. Add the new service to `PORT_BASES` in `scripts/wt.ts` so `wt ps` and
   `wt clean` know about it.
