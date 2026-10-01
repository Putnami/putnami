# Python Getting Started (experimental)

Use Python only when you deliberately want an experiment with a FastAPI service,
importable package, data workload, or automation component in the same Putnami
workspace graph as TypeScript and Go. Python is not a default path and makes no
compatibility or feature-parity promise with those surfaces.

Python is extension-first: Putnami gives explicitly configured Python projects
templates, dependency sync, lint, test, serve, Docker packaging, and workspace
orchestration. It does not provide a Python framework package family.

## Pick the project shape

| You need... | Template | What you get |
|-------------|----------|--------------|
| An experimental FastAPI service | `python-server` | ASGI app, uvicorn serve command, tests, Docker packaging |
| An experimental Python package | `python-library` | Importable module, packaging metadata, tests |

If you are starting from an empty directory:

```bash
putnami init
```

## Create the project

For a service:

```bash
putnami deps add @putnami/python
putnami extensions install
putnami projects create api --template python-server
putnami deps install
putnami test api
putnami serve api
```

For a library:

```bash
putnami projects create features --template python-library
putnami deps install
putnami test features
```

## What to look at first

Start with the project lifecycle:

1. [Extension](/docs/frameworks/python/extension) for the exact Python jobs and opt-in configuration.
2. [Templates](/docs/tooling-&-workspace/templates) for the generated service and library shapes.
3. [Workspace](/docs/tooling-&-workspace/workspace) to understand how Python projects participate in impacted work.
4. [Jobs & caching](/docs/tooling-&-workspace/jobs-&-caching) to keep feedback loops fast.

## Daily loop

```bash
putnami test api
putnami serve api
putnami lint,test,build --impacted
```

The Python toolchain stays Python-native, but the workflow stays workspace-native.

## Read next

- [Python extension](/docs/frameworks/python/extension) for uv, Ruff, pytest, serve, and Docker behavior.
- [TypeScript getting started](/docs/frameworks/typescript/getting-started) if the project needs a full web application surface today.
- [Go getting started](/docs/frameworks/go/getting-started) if the project needs a compiled backend service surface.
