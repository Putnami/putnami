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
- Regenerates `uv.lock` if the workspace changed
