---
order: 15
---

# Workspace Install

**Command:** `putnami workspace-install`

**Purpose:** Sync UV workspace configuration and update `uv.lock`.

## Usage

```bash
putnami workspace-install [options]
```

## Options

- `--force`: Regenerate `uv.lock` even if it already exists.

## What it does

- Discovers Python projects with `pyproject.toml` that are selected by the
  workspace configuration
- Updates the root `pyproject.toml` `tool.uv.workspace.members`
- Updates `tool.uv.sources` for workspace packages
- Runs the uv the CLI resolved for the release the workspace pins (see
  [uv Release](./configuration.md#uv-release)). The job installs no uv:
  `putnami install` provisions the pinned release under the Putnami home
  before the job starts
- Declares that release as `required-version = "==<version>"` in the
  `[tool.uv]` table of the root `pyproject.toml` when the file declares no
  `required-version`; `putnami install` then pins it in `putnami.lock.json`
- Regenerates `uv.lock` with that uv if the workspace changed

On a hosted run, the job writes no file and nothing installs a uv: the runner
must hold the pinned release on its `PATH` or under the Putnami home.

The job fails when no uv of the pinned release resolves, on a hosted run or
not. The message names the release and `putnami install`, which provisions it.
The job requests nothing and runs no other uv.

## When it fails

The job stops before it runs uv, and writes nothing, when the root
`pyproject.toml`:

- declares a `required-version` that names no single release, such as a range;
- declares a release older than 0.11.8;
- writes uv settings as dotted keys or an inline table instead of a
  `[tool.uv]` table.

The message names the declaration to write. See
[uv Release](./configuration.md#uv-release).

## Upgrading an existing workspace

A workspace that an older CLI installed has no `toolchains.uv` entry in
`putnami.lock.json`. Its Python tasks fail with
`workspace lock has no exact "uv" pin` until the lock pins one.

1. Upgrade the `putnami` CLI to a release that pins uv.
2. Run `putnami install` once. It pins the release the root `pyproject.toml`
   declares, or 0.11.32 when it declares none, provisions it under the Putnami
   home, and declares it as `required-version` when the file has none.
3. Commit `putnami.lock.json` and the root `pyproject.toml`.

A hosted run writes neither file. It needs that commit, and the pinned uv
release on the runner's `PATH` or under the Putnami home.

A CLI older than the uv pin never writes `toolchains.uv`, so with it the error
comes back after every `putnami install`: upgrade `putnami` and repeat step 2.
Once the pin is committed, an older CLI keeps it and runs the pinned release.
