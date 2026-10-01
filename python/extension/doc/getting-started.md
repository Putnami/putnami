---
order: 2
---

# Getting Started

`@putnami/python` provides **experimental**, explicit opt-in Python runtime
integration for Putnami workspaces. It is not a default path and makes no
compatibility or parity promise with Go or TypeScript.

## Prerequisites

- Python 3.11 or higher installed
- [uv](https://docs.astral.sh/uv/) installed
- A Putnami workspace

## Installation

The Python extension ships with `@putnami/putnami`, but it is not enabled by
default. Explicitly add it to the workspace before creating a Python project:

```bash
putnami deps add @putnami/python
putnami extensions install
```

You can instead register it in `putnami.workspace.json` and run
`putnami extensions install`.

## Project Structure

Python code lives alongside your TypeScript packages:

```text
packages/
├── my-typescript-lib/
│   ├── src/
│   └── package.json
└── api-server/
    ├── src/
    │   └── main.py
    ├── pyproject.toml
    └── putnami.json
```

## Experimental quick start

### 1. Scaffold a server

```bash
putnami projects create api-server --template python-server
```

### 2. Sync the UV workspace

```bash
putnami workspace-install --force
```

### 3. Run the server

```bash
putnami serve api-server --watch
```

## Manual setup (no template)

Create `packages/api-server/putnami.json`:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-project.json",
  "name": "api-server",
  "type": "application",
  "main": "src/main.py",
  "extensions": ["@putnami/python"]
}
```

Then register the project directory in the workspace `putnami.workspace.json` `includes` array:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-workspace.json",
  "includes": [
    "packages/api-server"
  ]
}
```

> The legacy `.putnamirc.json` filename is still read as a workspace-level fallback, but new projects and workspaces should use `putnami.json` and `putnami.workspace.json`.

Create `packages/api-server/pyproject.toml`:

```toml
[project]
name = "api-server"
version = "0.1.0"
requires-python = ">=3.11"
dependencies = [
  "fastapi>=0.110",
  "uvicorn>=0.27"
]
```

## Libraries

Scaffold a library:

```bash
putnami projects create py-lib --template python-library
```

Python import module names use underscores (e.g., `py-lib` → `py_lib`).

### Using a workspace library

Add the library to your consumer's `putnami.json` dependencies and sync:

```json
{
  "dependencies": [
    "packages/py-lib"
  ]
}
```

```bash
putnami workspace-install --force
```

## Standalone Usage

The Python extension can also be used without npm installation. Register it via
local path in the workspace `putnami.workspace.json` only when you explicitly
want this experimental surface:

```json
{
  "extensions": [
    "./path/to/@putnami/python"
  ]
}
```

The manifest includes embedded `flags` for all jobs, making it self-describing. CLI metadata is read directly from the manifest without importing TypeScript modules. See the [Extension SDK](../../../tooling/extension-sdk/README.md) for the shared protocol and runtime helpers.

Python jobs enter through the extension's prepared runtime, which invokes `uv`
for Python tool and workload processes. Bun is not required at runtime.

## Next Steps

- See [Configuration](./configuration.md) for runtime settings
- See [Serve](./serve.md) for execution options
