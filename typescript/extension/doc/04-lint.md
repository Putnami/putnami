# Lint

The lint command formats and lints TypeScript code using [Biome](https://biomejs.dev/). The no-fix (read-only) path runs a single `biome check` pass that combines formatting and linting in one file scan; the fix (writer) path runs the format and lint phases separately so auto-fix behavior is preserved exactly.

## Overview

- Formats code with Biome (consistent style, import ordering)
- Lints code with Biome (correctness, complexity, performance, security, style rules)
- Auto-fixes issues by default (`--fix`)
- Ships a zero-config Biome preset with sensible defaults
- Supports project-level and workspace-level config overrides

## Usage

### Basic Usage

```bash
# Format and lint with auto-fix (default)
putnami lint .
```

### Common Patterns

```bash
# Check only — report issues without fixing
putnami lint . --fix false

# Show more diagnostics
putnami lint . --max-diagnostics 100

# Show all severity levels (including info)
putnami lint . --diagnostic-level info

# Use a custom Biome config file or config directory
putnami lint . --config-path ./biome.json
```

## Pipeline

```text
--fix false (no-fix, read-only):   check (format + lint, one scan)
--fix true  (writer):              format ──→ check
```

### No-fix path (read-only)

`putnami lint . --fix false` — the canonical CI target — runs a single
`biome check` invocation that applies both the formatter and the linter in one
scan of the project source files. This replaces the earlier two-phase pipeline
(`biome format` followed by `biome lint`), which scanned and parsed the same
files twice. Collapsing to one pass is iso-functional — the same formatting and
lint rules are enforced — while roughly halving the per-job I/O and parse cost.

Assist enforcement is disabled (`--enforce-assist=false`) so the combined pass
fails (and reports) on exactly the checks the previous format and lint phases
did. With the default `warn` diagnostic level, assist findings (such as import
organization) are reported at info level and filtered out, matching the prior
behavior.

### Fix path (writer)

`putnami lint .` (default `--fix true`) keeps the format and lint phases
separate:

1. **Format** — `biome format --write` applies formatting fixes.
2. **Check** — `biome lint --write --unsafe` applies safe and unsafe lint fixes.
   Unsafe fixes include more aggressive transformations that may change
   semantics (e.g., simplifying control flow, removing unnecessary code).

The writer path is intentionally kept distinct from the combined no-fix pass
because `biome check --write` would also apply assist actions (e.g. reorder
imports), which the format + lint phases do not — keeping the phases separate
preserves auto-fix behavior exactly.

All lint phases key their cache on every project file type Biome can rewrite,
not only `src/`. This includes tests and project-root files, so a cache result
cannot let another worktree skip a required fix outside `src/`.

### Skip guard

A focused test can stop other tests from running, and a test skipped for no
reason, or because it is flaky or fails on CI, hides a failure. The skip guard
refuses both. It runs in the read-only `check` pass and in the writer's
`check` phase, never in the format phase, over the project's test files
(`*.test.*`, `*_test.*`, `*.spec.*`, `*_spec.*` with a `ts`, `tsx`, `js`,
`jsx`, `mts`, `cts`, `mjs` or `cjs` extension).

| Form | Result |
|------|--------|
| `.only(`, such as `test.only(`, `test['only'](` or `describe.each(cases).only(`, and `fit(` or `fdescribe(` | error |
| `.skip(` outside a guard, such as `it.skip(` at the top of a suite or `const later = test.skip`, and `xit(`, `xtest(` or `xdescribe(` | error |
| `.skipIf(`, `.if(`, or a guarding `if`, `case`, `&&` or ternary whose condition names CI or flakiness, at any depth, such as `describe.skipIf(!!process.env.CI)`, `process.env.CI && test.skip(`, `(isFlaky ? test.skip : test)(` or a `describe` inside `if (isCI)` | error |
| `.skipIf(<condition>)` or `.if(<condition>)` with any other condition, such as a platform (`process.platform`) or dependency check | allowed |
| `.skip(` inside an `if` block, a `switch`, a ternary, or after `&&`, when the condition does not name CI or flakiness | allowed |
| `.skip(` in the `else` branch of any condition, the second branch of a ternary, or after `\|\|` or `??`: the skip runs when the condition is false, as in Go | allowed |
| `.skip(` or `.only(` on something that is not a test API, such as a query builder's `qb.skip(20)` | allowed |

The guard walks out from each skip through every condition that holds it,
not only the nearest one: `isCI && (onWin && test.skip(...))` and
`if (isCI) { describe('a', () => { it.skip(...) }) }` are refused. A case
label counts for every statement of its clause and for the clauses that fall
through into it, up to a `break`, `return`, `throw` or `continue` that
always runs. The guard reads tokens, not a syntax tree: a case clause
that ends only in both branches of an if-else, or inside a try block, reads
as falling through into the next clause.

The guard does not judge what a condition checks: any condition that does not
name CI or flakiness counts as a guard. A `.skip(` or `.only(` is a test API
when its chain starts at `test`, `it`, `describe`, `suite`, `bench`,
`context`, `specify` or a name ending in `Test`, or when the call registers a
named case (a string name followed by a body). A string key reads the same
member: `test['skip']` is `test.skip`, and `test.skip?.(`, `test.skip!(` and
`test.skip<Ctx>(` are calls. An uncalled `test.skip` or `test.only` that is handed on to
register tests (assigned, returned, or chosen by a condition, as in
`const later = test.skip`) is the API itself, so the guard reads it too. An
argument such as `expect(test.skip)`, an assignment to `test.skip`, and
`typeof test.skip` are not read.

The test name is not a skip reason, so only the condition is read for CI or
flakiness. The guard reads tokens, not types: comments, strings, template
literals and regular expressions never match. It does not parse JSX text, so
an apostrophe in JSX text reads as a string to the end of its line. It skips
`node_modules`, `dist`, nested projects and the directories git ignores, the
same files the task's cache key leaves out.

A skip or focus that stays on purpose carries a reviewed exception, on the same
line or on a comment line of its own just above:

```ts
// putnami:allow-skip this case proves that specTest.skip registers a skipped case
specTest.skip('direct skipped case', binding, () => {});
```

The lint reports each reviewed exception as a warning, so the review stays
visible. An exception without a reason is an error.

The built-in Biome preset turns Biome's `noSkippedTests` rule off. That rule
warns on every skip, guarded or not, and its fix removes `.skip` when lint
writes, which turns a platform-guarded skip into a test that runs everywhere.
The skip guard replaces it. The writing pass also skips `noSkippedTests` and
`noFocusedTests`, so a workspace `biome.json` written before this change, or a
workspace where Biome turns `noFocusedTests` on for a test framework
dependency, never has `.skip` or `.only` removed. To drop the remaining
read-only warnings in such a workspace, set
`"linter": { "rules": { "suspicious": { "noSkippedTests": "off" } } }` in its
`biome.json`.

To turn the guard off for a workspace, set the option in `putnami.workspace.json`:

```json
{ "options": { "@putnami/typescript:lint": { "skip-guard": false } } }
```

The same key in a project's `putnami.json` turns it off for that project.

### Documentation links

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

### Nested projects

A subdirectory that holds its own `putnami.json` is a nested project, and
linting its parent never reads or rewrites it, within the limits below. When a project contains nested
projects, Putnami gives Biome the project's other files and directories instead
of the project root, still in one invocation; a project without nested
projects is passed as its root. Each nested project is linted by its own
extension, so a Go client generated under `clients/go` keeps the formatting of
its own toolchain. The scan for nested projects enters hidden directories but
skips the directories the cache key never reads (`node_modules`, `.git`,
`.putnami`, `out`, `dist` and `vendor`), which Biome receives whole. It does not
follow a symbolic link: Biome receives the link and may follow it. A
subdirectory the scan cannot read is passed to Biome whole, as before.

### Source mutation is declared, not inferred

The split between the read-only pass and the writer phases is stated in the
manifest rather than discovered after a job runs: the two writer tasks
(`lint-format`, `lint-check`) declare `mutatesSources` together with the
project-scoped `sources` write resource the planner serializes conflicting jobs
on, and the read-only task (`lint-all`) declares neither. None of the three
produces output files. That declaration is what lets the planner and the cache
know *before* a job runs that its inputs may not survive it.

After each successful writer phase, Putnami recomputes its keyed project-source
digest. An already-clean phase keeps the same digest and becomes a normal warm
cache hit; a phase that changed keyed sources is marked non-restorable, so
another worktree cannot receive green without applying the edits.

### Multi-project batching

When several selected projects use the same Biome configuration and become
ready together, Putnami sends their roots through one Biome invocation. The
read-only path performs one combined check for the group; the writer path
performs one format invocation for the group, waits for it to finish, then
performs one lint-fix invocation for the group.

Diagnostics and outcomes are attributed back to their project paths, and the
configured `--max-diagnostics` limit is applied independently to each project.
Cache lookups and entries also remain independent, so a warm project can hit
while a changed peer runs in the same dispatch. Projects that resolve different
configuration files, and extensions without the batchable task trait, continue
to run separately.

## Configuration Resolution

Biome configuration is resolved in this order:

1. **Project root** — `biome.json` in the project root
2. **Workspace root** — `biome.json` in the workspace root
3. **Extension default** — `node_modules/@putnami/typescript/config/biome.json`

The first `biome.json` found wins. To customize, place a `biome.json` in your project or workspace root.

`putnami deps install` materializes a missing workspace `biome.json` with the rules shipped by the resolved TypeScript extension and marks it as the workspace-root configuration. It preserves an existing workspace configuration byte-for-byte and stops with an explicit error if the installed extension default cannot be read, so every project resolves one stable workspace configuration.

## Biome Resolution

Lint needs Bun and no Node.js on the host. It looks in the nearest `node_modules`, from the project up to the workspace root, and starts the first of:

1. **The native executable** of the platform package installed for the host: `@biomejs/cli-<os>-<arch>/biome`, and the `-musl` package on a musl host. A hoisted install and an isolated install are both read.
2. **The launcher** in `node_modules/.bin`, when no platform package is installed for the host. The launcher is a JavaScript file: lint runs it with the Bun the task uses, never with a `node` from `PATH`.

Without a `node_modules` entry, lint starts the `biome` found on `PATH`.

### Default Configuration

The extension ships a comprehensive Biome config that covers TypeScript, JSON, CSS, HTML, and GraphQL. Key settings:

**Formatter:**
- 2-space indentation, 120-char line width, LF line endings
- Single quotes, trailing commas, semicolons always
- Import ordering: Bun → Node → packages → aliases → paths

**Linter rules (highlights):**

| Category | Key Rules |
|----------|-----------|
| Correctness | `noUnusedImports` (error), `noUnusedVariables` (error) |
| Complexity | `noExcessiveCognitiveComplexity` max 15 (warn), `noExcessiveLinesPerFunction` max 100 (warn) |
| Performance | `noAccumulatingSpread` (warn), `noAwaitInLoops` (warn) |
| Suspicious | `noConsole` (error), `noExplicitAny` (warn) |
| Style | `useFilenamingConvention` camelCase/PascalCase/kebab-case (warn) |

**Test file overrides:**
- `noExcessiveLinesPerFunction` is disabled for `*.test.ts` and `*.spec.ts` files

### Custom Configuration

To override the defaults, create a `biome.json` in your project root:

```json
{
  "$schema": "https://biomejs.dev/schemas/2.5.3/schema.json",
  "extends": ["../../node_modules/@putnami/typescript/config/biome.json"],
  "linter": {
    "rules": {
      "suspicious": {
        "noConsole": "off"
      }
    }
  }
}
```

## API Reference

### Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--fix` | `boolean` | `true` | Auto-fix issues. When `true`, runs the format + lint writer phases; when `false`, runs a single read-only `biome check` pass |
| `--config-path` | `string` | `node_modules/@putnami/typescript/config` | Path to a Biome config file, or to a directory containing `biome.json` |
| `--max-diagnostics` | `number` | `10` | Maximum number of diagnostics to display |
| `--diagnostic-level` | `string` | `warn` | Minimum severity to report: `error`, `warn`, or `info` |
| `--skip-guard` | `boolean` | `true` | Refuse a focused test and a test skip without a guard. See [Skip guard](#skip-guard) |
| `--docs-links` | `boolean` | `true` | Check every relative link and anchor in the project's documentation. See [Documentation links](#documentation-links) |

### Output

The pass emits structured diagnostics with:
- File path, line number, and column
- Severity (error, warning, info)
- Rule category and description
- Summary counts (errors, warnings, changed/unchanged/skipped files)

## Boundaries

- **Scope**: Formatting and linting TypeScript, JavaScript, JSON, CSS, HTML, and GraphQL files within a project
- **Out of scope**: Type checking (see [Build](./02-build.md) types phase), custom Biome plugins. Test rules other than the [skip guard](#skip-guard) come from Biome overrides
- **Dependencies**: Bun, and Biome resolved from `node_modules` (walks up the directory tree) or `PATH`; see [Biome Resolution](#biome-resolution). Node.js is not needed
- **Extension points**: Override Biome configuration by placing `biome.json` in project root or workspace root. Use `extends` to inherit the default config.
