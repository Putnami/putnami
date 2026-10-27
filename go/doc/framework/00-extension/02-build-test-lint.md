# Build, Test & Lint

The Go extension keeps Go's normal tools but runs them through Putnami jobs.

## Build

```bash
putnami build .
putnami build . --target linux/amd64
```

Build behavior:

- optionally refreshes dependencies unless `--skip-deps` is set
- compiles packages and binaries
- supports cross-compilation through `--target`
- writes output to the job output directory or `--output`

Useful options:

- `--target <os/arch>`
- `--output <path>`
- `--skip-deps`
- `--readonly`
- `--mod <readonly|vendor|mod>`
- `--ldflags`, `--gcflags`, `--asmflags`, `--tags`
- `--race`, `--trimpath`, `--buildmode`, `--cgo`, `--buildvcs`

## Test

```bash
putnami test .
putnami test . --coverage
putnami test . --coverage-threshold 80
```

Test behavior:

- runs Go tests with structured JSON output
- can generate coverage profile and HTML report
- parses results into Putnami diagnostics and metrics

Useful options:

- `--coverage`
- `--race`
- `--no-race-changed`
- `--timeout <duration>`
- `--run <pattern>`
- `--count <n>`
- `--shuffle`
- `--bench <pattern>`
- `--coverage-threshold <pct>`

### The race detector on changed packages

When a run measures a diff, such as `putnami test --impacted`, `test` also runs
`go test -race` on the packages the change touched, in a `test-race` step after
the test step passes:

- The packages are the directory of each changed Go file, and the package that
  holds a changed `testdata` directory, in the project that owns the file.
  Generated `.gen` code is left out.
- A project the change reaches only through a dependency runs no race step.
  A run without a diff, such as `--all` or `--projects`, runs none.
- The changed files key the step, so the same change reuses the stored
  verdict.
- The step measures no coverage. It reports its counts in a log line, never as
  the command's test totals. Its failures are diagnostics.
- `--timeout` does not apply to it. Its own deadline is its task's
  (`tasks["test~test-race"].timeoutMs` in `putnami.json`, 600000 ms by
  default), and its `go test -timeout` sits below that deadline.

`--race` replaces the step: the test step runs every package under the
detector. `--no-race-changed` turns the step off.

Off macOS, the race detector builds through cgo, so the step sets
`CGO_ENABLED=1` and needs the C compiler `go env CC` names on `PATH`. A host
that cannot run the detector fails the step with a message that names
`--no-race-changed`. Putnami does not cache that failure, so the next run tries
again, for example after you install a compiler. On macOS the detector needs no
C compiler.

## Lint

```bash
putnami lint .
putnami lint . --tool golangci-lint
```

Lint runs `golangci-lint`, `staticcheck`, or both.

With `golangci-lint`, lint also refuses a test that hides a failure: a
`t.Skip`, `t.Skipf` or `t.SkipNow` outside any `if`, `switch` case or `select`
case, or one whose reason or condition names flakiness or CI. Any other
condition, such as a platform check, a missing dependency or
`testing.Short()`, counts as a guard, and so does a build constraint that
names a platform. A skip that stays on purpose carries
`//putnami:allow-skip <reason>` on its line or on a comment line just above,
and lint reports it as a warning. Set
`"@putnami/go:lint": { "skip-guard": false }` under `options` in
`putnami.workspace.json` to turn the guard off.

Lint also checks every relative link and anchor in the project's `README.md`
files and `doc/` trees, and fails on one that does not resolve. Set
`"lint": { "docs-links": false }` under `options` in a project's
`putnami.json` to turn the check off for that project.

Config resolution for `golangci-lint`:

1. `--config`
2. project `.golangci.yml` or `.golangci.yaml`
3. workspace `.golangci.yml` or `.golangci.yaml`
4. bundled default config from `@putnami/go`
