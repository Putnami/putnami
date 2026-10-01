---
order: 13
---

# Serve Command

**Command:** `putnami serve [project]`

**Purpose:** Start a Python server (e.g., FastAPI, Flask, Django) using UV.

The resident server is always planned as `NO-CACHE`; a cache hit cannot
reproduce a live process or bound port.

## Usage

```bash
putnami serve <project> [options]
```

## Arguments

- `project`: The name or directory of the package to serve.

## Options

- `--entrypoint <path>`: Entry point to run (default: `src/main.py`).
- `--port <number>`: Port to bind (default: `3000`). Exposed as `PORT` environment variable.
- `--watch` / `--no-watch`: Restart the server on `.py` and `.toml` file changes (default: enabled).

## Examples

```bash
putnami serve api-server
putnami serve api-server --entrypoint src/main.py
putnami serve api-server --port 8080
putnami serve api-server --no-watch
```
