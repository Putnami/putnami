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

## uv Release

Every Python task runs one uv release: the one `putnami.lock.json` pins under
`toolchains.uv`, with its source and one SHA-256 per platform. The CLI resolves
it for each task and keys the task's cache on it. It takes the first of these
that reports the pinned release with `uv self version --short`:

1. `uv` on `PATH`
2. The uv the CLI provisioned, in `toolchains/uv/uv-<version>` under the
   Putnami home (`PUTNAMI_HOME`, else `~/.putnami`): the `uv` program of the
   pinned archive, at `uv-<target>/uv` for a tar.gz archive and `uv.exe` at the
   top for a zip archive

A uv of another release is never run. The extension installs no uv. When
neither holds the pinned release, `putnami install` downloads the pinned
archive, checks its SHA-256, unpacks it under the Putnami home, and a job runs
that uv. A hosted run installs nothing and runs the uv the runner holds. A job
whose uv does not resolve fails with a message that names the release and
`putnami install`.

The release comes from the root `pyproject.toml`:

```toml
[tool.uv]
required-version = "==0.11.32"
```

When the file declares no `required-version`, `putnami workspace-install`
declares the release it runs, 0.11.32 by default. To move to another
release, change `required-version` and run `putnami install`: it pins the new
release and provisions it.

The declaration must name one exact release, `"==X.Y.Z"`. Putnami refuses
these, and `putnami install` and every task that runs uv fail with a message
that names the declaration to write:

| Declaration | Why it is refused |
|-------------|-------------------|
| A range or a wildcard, such as `">=0.11"` or `"==0.11.*"` | It names no single release to pin |
| A value that is not a one-line string | It names no release |
| A release older than 0.11.8 | An older uv prints more than its release with `uv self version --short`, so Putnami cannot tell which release it runs |

`putnami workspace-install` also refuses a root `pyproject.toml` that writes uv
settings outside a `[tool.uv]` table header, as dotted keys (`uv.managed = true`
under `[tool]`) or as an inline table (`uv = { managed = true }`). A
`[tool.uv]` table added for `required-version` would define the table twice.
Move those settings into a `[tool.uv]` table.

The extension answers the `__putnami toolchain-resolve` control call for the
`uv` lock identity, which its manifest lists in `runtime.resolves`. The answer
is the next `toolchains.uv` pin: each platform's archive URL, format and `uv`
executable path, with its SHA-256. The CLI downloads, verifies and unpacks
those archives; the extension does not. The release comes from the `[tool.uv]`
`required-version` of the root `pyproject.toml`. The lock's pin is kept when
it matches that declaration, or when the file declares none. With neither a
declaration nor a pin, the answer pins the default release, 0.11.32. The
default release is pinned offline from the digests the extension ships. The
extension reads a release's checksum file only for a declared release that is
neither pinned nor the default, and never on an offline request, where that
release is a problem whose remedy is `putnami install` with network access.
The answer is also a problem, with a remedy, for a declaration Putnami cannot
pin and for a host platform uv publishes no archive for.

A workspace that a CLI older than the uv pin installed has no `toolchains.uv`
in its lock. See
[Upgrading an existing workspace](./workspace-install.md#upgrading-an-existing-workspace).

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
