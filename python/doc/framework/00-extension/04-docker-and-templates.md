# Docker & Templates

The experimental Python extension provides explicit templates and Docker
packaging for Python services and libraries. These templates are not defaults
and do not promise Go or TypeScript parity.

## Templates

| Template | What you get |
|----------|--------------|
| `python-server` | FastAPI server, uvicorn serve command, tests, Docker-ready shape |
| `python-library` | Importable Python module, packaging metadata, tests |

```bash
putnami deps add @putnami/python
putnami extensions install
putnami projects create api --template python-server
putnami projects create features --template python-library
putnami deps install
```

Python module names are normalized with underscores, so `my-lib` becomes `my_lib`.

## Docker

When the `docker` publish channel is enabled, the extension generates a Dockerfile based on `ghcr.io/astral-sh/uv:python3.12-bookworm-slim` and uses uv for dependency resolution.

```json
{
  "publish": ["docker"]
}
```

Use the generated project shape and Putnami publish flow instead of hand-maintaining per-project Dockerfiles.
