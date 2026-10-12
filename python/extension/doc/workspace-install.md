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
- Selects the uv release the workspace pins (see
  [uv Release](./configuration.md#uv-release)) and installs it under the
  Putnami home when the machine holds none
- Declares that release as `required-version = "==<version>"` in the
  `[tool.uv]` table of the root `pyproject.toml` when the file declares no
  `required-version`; `putnami install` then pins it in `putnami.lock.json`
- Regenerates `uv.lock` with that uv if the workspace changed

On a hosted run, the job installs no uv and writes no file: the runner must
hold the pinned release on its `PATH` or under the Putnami home.
