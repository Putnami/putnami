---
order: 3
---

# Configuration

Configure experimental Python job behavior per explicitly opted-in project via
`putnami.json` options or CLI flags. These options do not make Python a default
workspace path or add parity with Go or TypeScript.

## Per-Project: Serve Defaults

Set default `serve` options so you don't need to pass flags every time:

```json
{
  "name": "api-server",
  "type": "application",
  "extensions": ["@putnami/python"],
  "options": {
    "@putnami/python:serve": {
      "entrypoint": "src/api_server/__init__.py",
      "port": 8080
    }
  }
}
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `entrypoint` | `string` | `src/main.py` | Python file to run as the server entrypoint |
| `port` | `number` | `3000` | Port to bind. Exposed as `PORT` environment variable |
| `watch` | `boolean` | `true` | Restart on `.py`/`.toml` file changes |

## Per-Project: Lint Defaults

```json
{
  "options": {
    "@putnami/python:lint": {
      "fix": false
    }
  }
}
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `fix` | `boolean` | `true` | Auto-fix issues with `ruff --fix` and format with `ruff format` |

## Per-Project: Test Defaults

```json
{
  "options": {
    "@putnami/python:test": {
      "log": true
    }
  }
}
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `log` | `boolean` | `false` | Enable pytest live logging (`-s --log-cli=true`) |
| `update-snapshots` | `boolean` | `false` | Pass `--snapshot-update` to pytest |

## Dependencies

All dependencies live in `pyproject.toml` under `[project].dependencies` (runtime) and `[project.optional-dependencies].dev` (dev tools):

```toml
[project]
name = "api-server"
version = "0.1.0"
requires-python = ">=3.11"
dependencies = [
  "fastapi>=0.110",
  "uvicorn>=0.27"
]

[project.optional-dependencies]
dev = [
  "pytest>=8.0",
  "httpx"
]
```

After adding or changing dependencies, sync the workspace lockfile:

```bash
putnami workspace-install --force
```

## Ruff Configuration

Ruff settings live in `pyproject.toml` under `[tool.ruff]`:

```toml
[tool.ruff]
line-length = 100

[tool.ruff.lint]
select = ["E", "F", "I"]
ignore = ["E501"]
```

The extension passes `--cache-dir .putnami/bin/extensions/putnami-python/.ruff_cache` automatically.

### Batching and configuration discovery

Both Ruff phases are batchable — the fixing tasks (`lint-format-fix`,
`lint-check-fix`) and their read-only counterparts (`lint-format-readonly`,
`lint-check-readonly`) alike: compatible projects are linted in a single Ruff
invocation. Projects are grouped by their effective Ruff
configuration, discovered (in order) from these candidates:

- `ruff.toml` or `.ruff.toml` in the project root
- `ruff.toml`, `.ruff.toml`, or `pyproject.toml` in the workspace root

The first existing candidate provides the group's configuration digest, so
projects with a dedicated project-level `ruff.toml`/`.ruff.toml` run on their
own while projects inheriting the workspace configuration batch together.

The project-level `pyproject.toml` is deliberately **not** a grouping candidate:
every UV workspace member has one at a distinct path, so including it would give
every project a distinct digest and defeat batching entirely. This costs no
correctness — the grouped invocation passes no `--config`, so Ruff still
resolves each file's configuration by walking up the directory tree. A project
whose own `pyproject.toml` carries a `[tool.ruff]` section is therefore still
linted with that configuration even when grouped, and nested `pyproject.toml`,
`ruff.toml`, and `.ruff.toml` files all remain correct.

## Pytest Configuration

Pytest settings live in `pyproject.toml` under `[tool.pytest.ini_options]`:

```toml
[tool.pytest.ini_options]
testpaths = ["tests"]
addopts = "--tb=short"
```

The extension passes `--cache-dir .putnami/bin/extensions/putnami-python/.pytest_cache` automatically.

## uv Directories

Every uv command the extension runs writes its managed Python installations and
its package cache inside the workspace, not under your home directory:

| Variable | Set to |
|----------|--------|
| `UV_PYTHON_INSTALL_DIR` | `.putnami/cache/extensions/@putnami-python/uv/python` |
| `UV_CACHE_DIR` | `.putnami/cache/extensions/@putnami-python/uv/cache` |

Jobs therefore run where the home directory is read-only, such as a CI runner.
All jobs of a worktree share these directories, so uv downloads a Python at most
once per worktree. uv downloads one only when no installed Python satisfies
`requires-python`.

If you set either variable yourself, the extension keeps your value.

## Next Steps

- See [Getting Started](./getting-started.md) for project setup
- See [Workspace Install](./workspace-install.md) for UV sync and lock behavior
- See [Serve](./serve.md) for serve-specific options
