---
order: 5
---

# Templates

The two Python templates are **experimental, non-default, and non-parity**.
First opt the workspace into the Python extension; neither template is a
recommended default starter or equivalent to a Go or TypeScript template.

```bash
putnami deps add @putnami/python
putnami extensions install
```

## `python-server` (experimental)

A FastAPI HTTP server with uvicorn, pytest, and httpx.

```bash
putnami projects create api-server --template python-server
```

**Generated structure:**

```text
api-server/
├── putnami.json            # type: application, serves from src/<module>/__init__.py
├── pyproject.toml          # fastapi, uvicorn, pytest, httpx
├── src/
│   └── api_server/
│       └── __init__.py     # FastAPI app with GET / health endpoint + main()
└── tests/
    └── test_api_server.py  # TestClient health endpoint test
```

**What you get:**

- `GET /` returns `{"Hello": "World"}` (replace with your routes)
- `main()` starts uvicorn on `0.0.0.0:8000`
- `tests/test_api_server.py` uses FastAPI's `TestClient` for in-process testing
- `httpx` included as dev dependency for the TestClient

**After scaffolding:**

```bash
putnami workspace-install --force   # sync UV workspace
putnami serve api-server            # start with hot-reload
putnami test api-server             # run tests
```

## `python-library` (experimental)

A Python importable package using setuptools.

```bash
putnami projects create py-lib --template python-library
```

**Generated structure:**

```text
py-lib/
├── putnami.json            # type: library
├── pyproject.toml          # setuptools, pytest
├── src/
│   └── py_lib/
│       ├── __init__.py     # exports hello()
│       └── hello.py        # hello(name: str) -> str
└── tests/
    └── test_py_lib.py      # tests hello()
```

**What you get:**

- `hello(name)` starter function to replace with your own code
- `src/` layout with `setuptools` auto-discovery
- Pytest test file ready to extend

**After scaffolding:**

```bash
putnami workspace-install --force   # sync UV workspace
putnami test py-lib                 # run tests
```

## Template Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `projectName` | Project name as-is | `api-server` |
| `projectModule` | Project name as Python module identifier | `api_server` |

The `projectModule` variable converts hyphens to underscores, making it a valid Python import name.

Both template records are classified `experimental`, `default: false`, and
`parity: unsupported` in the workspace [support catalog](../../../putnami.support.json).
Their exact outcomes and limits are documented in the
[server specification](../specs/experimental-server-template.json)
and [library specification](../specs/experimental-library-template.json).

## Using a Library from Another Project

After scaffolding a library and syncing the workspace, add it as a dependency in your consumer project's `putnami.json`:

```json
{
  "dependencies": ["packages/py-lib"]
}
```

Then sync again:

```bash
putnami workspace-install --force
```

UV workspace sources will make `py-lib` importable as `import py_lib` in any other workspace project.
