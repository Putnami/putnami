---
order: 11
---

# Lint Command

**Command:** `putnami lint [project]`

**Purpose:** Lint Python code with Ruff, and check the links in the project's documentation.

## Usage

```bash
putnami lint <project> [options]
```

## Arguments

- `project`: The name or directory of the package to lint.

## Options

- `--fix` / `--no-fix`: Enable or disable auto-fixes (default: `--fix`).
- `--docs-links` / `--docs-links=false`: Check every relative link and anchor in
  the project's documentation (default: on). See
  [Documentation links](#documentation-links).

## Examples

```bash
putnami lint api-server
putnami lint api-server --no-fix
```

## Documentation links

`lint` checks every relative link and anchor in the project's `README.md`
files and `doc/` trees. A link to a file that does not exist, a path whose case
differs from the name on disk, or an anchor that names no heading of its
Markdown target is an `error` diagnostic coded `docs-links`, and it fails the
task. Links to web pages and site routes are not checked. The step is
uncacheable: a link may name any file of the workspace.

Turn it off for one run with `--docs-links=false`, or for a project with
`"options": { "lint": { "docs-links": false } }` in `putnami.json`, which
also takes the project out of the link check `validate-workspace` runs. The rule
itself, shared by every language extension, is documented in the
[extension SDK](../../../tooling/extension-sdk/docslinks/README.md).

## Multi-project batching

When several selected projects resolve the same effective Ruff configuration and
become ready together, Putnami runs Ruff **once** over all of their directories
instead of once per project. Each Ruff phase runs a single grouped invocation;
format-before-check ordering is preserved by the task dependency graph, and
`--fix` writes are applied to every grouped project.

## Check/fix task split

Each Ruff phase is two tasks, and the pipeline picks one of them from `--fix`:

| `--fix` | format phase           | check phase           | Rewrites sources |
| ------- | ---------------------- | --------------------- | ---------------- |
| on      | `lint-format-fix`      | `lint-check-fix`      | yes              |
| off     | `lint-format-readonly` | `lint-check-readonly` | no               |

Only the two fixing tasks declare source mutation (the v3 `mutatesSources`
contract plus the project-scoped `sources` write resource the planner serializes
concurrent jobs on). A `--no-fix` run therefore no longer claims a source write
it does not perform, so it can run alongside jobs that read the same files. Both
halves resolve the same Ruff configuration candidates and batch identically.

After each successful fixing phase, Putnami recomputes its keyed project-source
digest. A clean phase keeps the same digest and becomes reusable on a warm lint
run; a phase that changed keyed sources is marked non-restorable, so every
worktree still applies its own Ruff edits.

Findings, summaries, statuses, and cache entries are attributed back to their
owning project by file path, so a clean project never inherits a sibling's
failure and a warm project can hit the cache while a changed peer runs in the
same dispatch. Ruff still resolves configuration per file by walking up the tree
(no `--config` override is passed), so nested `pyproject.toml`, `ruff.toml`, and
`.ruff.toml` are honored exactly as in a single-project run. Projects that
resolve a different effective configuration fall into separate groups and run on
their own.
