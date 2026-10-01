# Build, Test & Lint

The fast feedback loop for TypeScript projects is `build`, `test`, and `lint`. All three commands run inside the Putnami project graph, so dependency order and cache reuse stay consistent across the workspace.

## Build

Build compiles TypeScript packages for distribution with Bun and TypeScript declaration output.

Default build phases:

1. `generate` - pre-build hooks and generated artifacts
2. `transpile` - JavaScript package output in `output/lib`
3. `types` - declaration output in `output/types`

`compile` is opt-in and creates standalone executable output.

```bash
putnami build .
putnami build . --compile
putnami build . --compile --compile-target bun-linux-x64
```

Common options:

- `--generate`, `--transpile`, `--types`, `--compile` to select phases
- `--target <bun|browser|node>`
- `--bundle <standalone|local|none>`
- `--sourcemap <inline|external|none>`
- `--minify <boolean>`
- `--fast` to skip type generation
- `--clear` to wipe the output folder first

## Test

Tests run through Bun's test runner.

```bash
putnami test .
putnami test . --coverage
```

Behavior:

- discovers `**/*.{test,spec}.{ts,tsx,js,jsx}`
- returns `SKIP` when no tests are found
- produces JUnit output
- can produce LCOV coverage

Useful options:

- `--timeout <ms>`
- `--coverage`
- `--coverage-threshold <pct>`
- `--test-name-pattern <regex>`
- `--update-snapshots`
- `--pass-with-no-tests <boolean>`

## Lint

Lint uses Biome for format and static checks.

```bash
putnami lint .
putnami lint . --fix false
```

Config resolution:

1. project `biome.json`
2. workspace root `biome.json`
3. built-in config from `@putnami/typescript`

Lint also refuses a test that hides a failure: `.only(` (and `fit(`), a
`.skip(` outside a guard (and `xit(`), and a `.skipIf(`, `.if(`, `if`,
`case`, `&&` or ternary whose condition names flakiness or CI. A string key,
`test['only'](`, counts as `.only(`. Any other condition, in
`skipIf(<condition>)`, an `if`, an `&&` or a ternary, counts as a guard. A skip that stays on purpose carries
`// putnami:allow-skip <reason>` on its line or on a comment line just above,
and lint reports it as a warning. Set
`"@putnami/typescript:lint": { "skip-guard": false }` under `options` in
`putnami.workspace.json` to turn the guard off.

Lint also checks every relative link and anchor in the project's `README.md`
files and `doc/` trees, and fails on one that does not resolve. Set
`"lint": { "docs-links": false }` under `options` in a project's
`putnami.json` to turn the check off for that project.
