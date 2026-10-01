---
order: 1
---

# Python Extension

`@putnami/python` is an **experimental**, explicit opt-in workspace
integration. It is not a default path and has no parity or compatibility
promise with Go or TypeScript. It coordinates Python projects with uv rather
than providing a Python framework.

## What it provides

- **CLI jobs**: `lint`, `test`, `serve`, `workspace-install`
- **Workspace adapter**: answers the workspace probe for explicitly configured
  Python projects with each project's `[project]` name, and owns the
  `pyproject.toml` identity write (`workspace-sync`)
- **UV workspace sync**: discovers configured Python packages and updates root
  `pyproject.toml`
- **Templates**: scaffold Python servers and libraries

## Templates

- [`python-server`](./05-templates.md#python-server-experimental): experimental FastAPI
  starter with uvicorn, pytest, and a root-response test
- [`python-library`](./05-templates.md#python-library-experimental): experimental library
  skeleton with `src/` layout, setuptools, and tests

See [Templates](./05-templates.md) for generated structure and workspace library usage.

## How it works

- Each Python project is a normal package with a `pyproject.toml` `[project]` section.
- The extension answers the CLI's **workspace probe** with what each project's
  `pyproject.toml` declares. The CLI no longer reads `pyproject.toml` itself, so
  this is the only thing that gives a Python project its distribution identity.
  A manifest with no `[project] name` is reported as a diagnostic rather than
  guessed at.
- The workspace root `pyproject.toml` is updated with members and sources for
  explicitly configured Python packages.
- `putnami workspace-install` regenerates `uv.lock` when the workspace changes.

The [support catalog](../../../putnami.support.json) is the canonical status
authority. See the [experimental Python surface ADR](adr/0001-experimental-python-surface.md)
for the boundary and durable consequences.
