# How `putnami observe` reads a Python repository

The Python detector finds the projects of a repository from its
`pyproject.toml` files. It reads files only: it runs no `uv`, Poetry, `pip`
or `pytest`, and reads no virtual environment. The change analyses use what
it finds to name the changed and dependent projects, attribute tests and
coverage, and diff the `uv.lock` and `poetry.lock` versions.

## Components

Each Python project is a component of ecosystem `python`:

- When the root `pyproject.toml` has a `[tool.uv.workspace]` table, each
  directory its `members` globs match, less those its `exclude` globs match,
  that holds a `pyproject.toml` is a component of source `workspace`. A glob
  segment is `*`, `?` or `[…]`, or `**` for any number of directories.
  Only the root's workspace is read: a `[tool.uv.workspace]` in a
  subdirectory makes its projects manifest components, without its sources. The root
  is one too when it has a `[project]` table; a virtual workspace root, with
  no `[project]`, is none.
- Any other `pyproject.toml` with a `[project]` or a `[tool.poetry]` table is
  a component of source `manifest`. A `pyproject.toml` that only configures
  tools, such as `[tool.ruff]`, is none.

The search skips `node_modules`, `vendor`, `__pycache__`, `site-packages` and
the directories whose name starts with `.`, such as `.git` and `.venv`. A
component's id is its directory relative to the repository root, `.` for the
root, and its name the `[project]` name, else the `[tool.poetry]` name. A
file belongs to the innermost component whose directory holds it, so the
`uv.lock` of a virtual workspace root belongs to no component.

## Dependency edges

A project depends on another project of the repository through a dependency
it declares, in `[project]` `dependencies` or `optional-dependencies`, in a
`[dependency-groups]` group, or in a Poetry dependency table, when:

- its own `[tool.uv.sources]` entry for that dependency, else the workspace
  root's for a workspace member, has `workspace = true`: the edge goes to the
  member whose project name it is, both names normalized, so `Shared_Lib`
  matches `shared-lib`;
- that source has a local `path`, such as `{ path = "../shared" }`, resolved
  against the directory of the file that declares it;
- a Poetry dependency, in `[tool.poetry.dependencies]`,
  `[tool.poetry.dev-dependencies]` or a `[tool.poetry.group.<name>.dependencies]`
  table, has a local `path`.

A registry, `git` or `url` source is no edge, nor is a path outside the
repository or to a directory that is no component. A requirement such as
`shared @ file:///…` is not read.

For example, in this uv workspace `api` and `worker` depend on `shared`:

```toml
# pyproject.toml
[tool.uv.workspace]
members = ["api", "worker", "shared"]

[tool.uv.sources]
shared = { workspace = true }
```

```toml
# api/pyproject.toml
[project]
name = "api"
dependencies = ["fastapi>=0.115", "shared"]
```

A change to `shared` marks `api` and `worker` dependent.

## Reports

Run pytest with a JUnit and a Cobertura report, at the repository root or in a
project:

```bash
uv run --all-packages pytest --junitxml=junit.xml --cov --cov-report=xml
```

pytest writes each case's class name from its test file's path relative to
its `rootdir`, such as `api.tests.test_api.TestQuote` for
`api/tests/test_api.py`, and the change analyses read it as that file,
relative to the repository root, to the report's project or to the report's
directory. pytest picks its `rootdir` from its arguments and configuration
files: when it differs from those directories, as with `pytest api` run at
the root of a repository whose `api` holds the pytest configuration, pass
`--rootdir=.`, or write the report in the `rootdir`. pytest-cov writes `coverage.xml` with the
directory pytest ran in as its source root. See
[tests and coverage](06-change-analyses.md#tests-and-coverage).

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.python_manifest_invalid` | A `pyproject.toml` cannot be parsed: that file is no component. Or a `members` or `exclude` pattern of the uv workspace is not a repository-relative glob: it is ignored. |
| `observe.python_walk_bounded` | The search for `pyproject.toml`, or for the uv workspace members, stopped after 200,000 entries or could not walk the repository. |
