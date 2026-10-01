---
order: 4
---

# Jobs

The experimental Python extension exposes four user-facing jobs via the CLI, plus one internal
workspace job.

## Available Jobs

| Job | Command | Scope | Cached | Description |
|-----|---------|-------|--------|-------------|
| `lint` | `putnami lint <project>` | Project | Yes | Format and check with Ruff |
| `test` | `putnami test <project>` | Project | Yes | Run pytest |
| `serve` | `putnami serve <project>` | Project | No | Run a Python server via UV |
| `workspace-install` | `putnami workspace-install` | Workspace | No | Sync UV workspace + lock |
| `workspace-sync` | internal (`putnami projects sync`) | Workspace | No | Align the `pyproject.toml` `[project] name` of each project whose declared identity diverges from its resolved name |

## Job Details

### `lint`

Runs in three phases:
1. **format** — `ruff format` rewrites files in place (or checks with `--no-fix`)
2. **check** — `ruff check` reports lint violations (and auto-fixes with `--fix`)
3. **docs** — `lint-docs` fails on a relative link or an anchor in the project's
   `README.md` files and `doc/` trees that does not resolve (off with
   `--docs-links=false`; see [Lint Command → Documentation links](./lint.md#documentation-links))

The Ruff phases share the same `--fix` flag. `putnami lint` implicitly triggers `workspace-install` first.

```bash
putnami lint <project>           # format + fix
putnami lint <project> --no-fix  # check-only, no writes
```

When multiple compatible projects lint together, both phases are **batchable**:
Ruff runs once over the whole group (grouped by effective Ruff configuration)
and the results are split back per project. Each project keeps an independent
cache entry, so warm projects still hit the cache while changed peers run in the
shared invocation. See [Lint Command → Multi-project batching](./lint.md).

### `test`

Runs pytest. Auto-skips if no test files (`test_*.py` or `*_test.py`) are found.

```bash
putnami test <project>
putnami test <project> --log          # enable live logging
putnami test <project> -t test_health # filter by name/path
```

### `serve`

Runs the project's entrypoint via `uv run`. Defaults to watch mode — restarts on `.py` or `.toml` changes.

```bash
putnami serve <project>
putnami serve <project> --no-watch   # run once, no restart
putnami serve <project> --port 8080
```

### `workspace-install`

Discovers explicitly configured Python projects, syncs the root `pyproject.toml`
members and sources, then runs `uv lock` if the workspace changed. It does not
turn Python into a workspace default or create an unconfigured project.

```bash
putnami workspace-install           # only locks if workspace changed
putnami workspace-install --force   # always regenerates uv.lock
```

This job runs automatically before lint, test, and serve.

## Notes

- `workspace-install` is workspace-scoped: it runs once for the whole workspace, not per-project.

### `workspace-sync`

Internal. `putnami projects sync` runs it once for the whole workspace with the
resolved project selection, and it writes the resolved name into
`pyproject.toml` `[project] name` for the projects whose **declared** identity
diverges from that resolved name.

The resolved name is the CLI's answer — explicit `putnami.json` identity, the
scope `namePattern`, and the directory fallback have all been applied — so this
task never re-derives it. The divergence set is the CLI's answer too: each
selected project carries a `sourceName`, the identity it declared before any
scope `namePattern` override, and a project whose `sourceName` already equals
its resolved name is **left alone**. That manifest is where the identity came
from, so it is not out of alignment; rewriting it would rename the distribution
while every dependent still requires the old name. A selection that carries no
`sourceName` at all (an orchestrator older than the member) is treated the same
way — no rename — because not renaming is recoverable and renaming is not.

Only the `[project]` table is touched; a `name` under `[tool.*]` is a different
key, and a manifest that declares no `[project] name` is left alone rather than
having one invented for it. Under `--dry-run` the change is reported and nothing
is written.

The root `pyproject.toml`'s `[tool.uv.workspace]` and `[tool.uv.sources]` tables
stay with `workspace-install`: splitting one file's tables across two tasks is
how two writers start flipping it on alternate runs.
- `lint` and `test` results are cached against source files, `pyproject.toml`, and `uv.lock`. The `lint-docs` phase is not cached: a link may name any file of the workspace.
- The Ruff phases are batchable: compatible projects share one Ruff invocation while keeping independent per-project caches.
- Each Ruff phase is split into a fixing and a read-only task (`lint-format-fix`/`lint-format-readonly`, `lint-check-fix`/`lint-check-readonly`); the pipeline schedules one half based on `--fix`, and only the fixing half declares source mutation.
- `serve` has no timeout (runs until killed or the process exits).
