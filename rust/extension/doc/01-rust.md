# @putnami/rust reference

`@putnami/rust` turns each crate of a Rust repository into a Putnami project
and runs its lint, test and build with the cargo tools the team already uses.
It reads the repository the way `putnami observe` does, needs no
`putnami.json` in a typical repository, and never installs a toolchain.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/rust"] }
   ```

2. Run `putnami projects sync`. Each directory that holds a `Cargo.toml` with
   a `[package]` table, or that a `[workspace]` names as a member, joins the
   workspace's `includes`, and so does each virtual Cargo workspace below the
   workspace root. The extension names each crate by its package, adds an edge
   to each crate it depends on by path, and tags it `rust`.
3. Install cargo-nextest and cargo-llvm-cov, and give the nextest profile `ci`
   a JUnit report, in `.config/nextest.toml` at the Cargo workspace root:

   ```toml
   [profile.ci.junit]
   path = "junit.xml"
   ```

4. Run `putnami lint,test,build`. A second run over an unchanged tree is a
   cache hit for every crate.

## Crates and stack roots

A crate's **stack root** is the directory of the Cargo workspace that holds
it: its `Cargo.toml`, `Cargo.lock`, `.cargo/config.toml` and `target/`.

| Crate | Stack root |
|---|---|
| A member of the workspace root's `[workspace]`, or the root package | The workspace root |
| A member of a nested `[workspace]`, the nearest one above the crate, or the one its `package.workspace` names | That workspace's directory |
| A crate that declares its own `[workspace]`, or that no workspace names | Its own directory |

A virtual Cargo workspace below the workspace root, such as
`services/billing` with the members `services/billing/core` and
`services/billing/http`, is an **input-only project**: its tasks run no step,
and each task reads the root files it needs. Each of its crates depends on
it, so a change to `services/billing/Cargo.lock` reruns their test and build,
and not their lint. A member of a nested workspace whose root package depends
on it is left out with `stacks.stack_root_unsupported`: the edge to its root
would close a cycle.

The crates of one stack root run one task at a time, because they share its
`target/` directory, and a task of a crate waits for the same task of the
crates it depends on.

## What each task runs

Each task runs in the crate's directory, with no shell. `{component}` is the
crate's package name, `{root}` its stack root, `{output}` the task's output
directory.

| Task | Steps |
|---|---|
| `lint` | `cargo fmt --package {component} --check`, then `cargo clippy --package {component} --all-targets -- -D warnings` |
| `test` | `cargo llvm-cov nextest --package {component} --profile ci --lcov --output-path {output}/lcov.info` |
| `build` | `cargo build --package {component} --locked` |

After its steps, `build` copies `{root}/target/debug/{component}`, the
crate's binary, into its output when cargo wrote one there, so a cache hit
restores it. A library crate, or a target directory moved by
`CARGO_TARGET_DIR` or `build.target-dir`, copies nothing.

## What the test task reads

The test task reads `{root}/target/nextest/ci/junit.xml` and its LCOV report
into the test and coverage summary. A report left at the root by another crate
or an earlier run is removed before the steps, so it is never read as this
crate's.

| Entry | Attributed to | When it names none |
|---|---|---|
| A JUnit case | The crate its nextest binary id names, its class name else its suite name: `pricing` for `pricing::integration`, `api` for `api::bin/api`, with `-` and `_` read as one | Counted, with the warning `stacks.test_unattributed` |
| A coverage file | The crate whose directory holds it, the deepest one: an absolute path as cargo-llvm-cov writes it, or a path relative to the crate | Left out of the coverage, with the warning `stacks.coverage_unmapped` |

The cases and coverage files of another crate are left out of the summary:
that crate's own task counts them. A failed case, a step that exits non-zero,
or a missing report fails the task.

## The toolchain check

Before its steps, each task compares `rustc --version` with the version that
`rust-toolchain.toml` (`toolchain.channel`) or `rust-toolchain` states, in the
crate's directory, then at its stack root.

| Case | Result |
|---|---|
| The versions match | The task runs; a `1.82` channel matches the stable 1.82 releases, such as 1.82.3, and keys the minor release |
| They differ | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| `rustc` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| No file states an exact version, such as `channel = "stable"` | The warning `stacks.toolchain_unpinned`: the cache key carries no toolchain version |

## What reaches the cache key

A task's key covers the files its input sets select, the crate's
`putnami.json`, the toolchain files, the expected toolchain version, and the
key of the same task of each crate it depends on. A pattern that starts with
`{root}/` is a file of the stack root.

| Input set | Patterns |
|---|---|
| `sources` | `src/**`, `build.rs` |
| `targets` | `tests/**`, `benches/**`, `examples/**` |
| `manifests` | `Cargo.toml`, `.cargo/config.toml`, `.cargo/config`, in the crate and at the stack root |
| `lintConfig` | `rustfmt.toml`, `.rustfmt.toml`, `clippy.toml`, `.clippy.toml`, in the crate and at the stack root |
| `testConfig` | `{root}/.config/nextest.toml` |
| `lockfiles` | `Cargo.lock`, in the crate and at the stack root |

| Task | Reads | Does not rerun on |
|---|---|---|
| `lint` | `sources`, `targets`, `manifests`, `lintConfig` | A `Cargo.lock` or nextest configuration change |
| `test` | `sources`, `targets`, `manifests`, `testConfig`, `lockfiles` | A rustfmt or clippy configuration change |
| `build` | `sources`, `manifests`, `lockfiles` | A change to a test, a bench, an example, or a lint or nextest configuration |

## Common overrides

A crate overrides any value of the profile in its own `putnami.json`, under
`options["@putnami/rust"]`. An object merges; any other value, such as a list
of steps or patterns, replaces the profile's whole. The override reaches the
cache key.

Test with a feature, or with every feature:

```json
{ "options": { "@putnami/rust": { "tasks": { "test": { "steps": [
  ["cargo", "llvm-cov", "nextest", "--package", "{component}", "--all-features", "--profile", "ci", "--lcov", "--output-path", "{output}/lcov.info"]
] } } } } }
```

Read a file outside the default sets, such as test fixtures or SQL
migrations embedded with `include_str!`:

```json
{ "options": { "@putnami/rust": { "inputs": { "sources": ["src/**", "build.rs", "migrations/**"] } } } }
```

Keep a binary whose name is not the package's, from a `[[bin]]` target:

```json
{ "options": { "@putnami/rust": { "tasks": { "build": { "artifacts": ["{root}/target/debug/billing-server"] } } } } }
```

Lint without failing on clippy warnings, while a codebase catches up:

```json
{ "options": { "@putnami/rust": { "tasks": { "lint": { "steps": [
  ["cargo", "fmt", "--package", "{component}", "--check"],
  ["cargo", "clippy", "--package", "{component}", "--all-targets"]
] } } } } }
```

Run on a toolchain other than the pinned one, with a warning:

```json
{ "options": { "@putnami/rust": { "toolchain": { "check": "warn" } } } }
```

A steps override keeps the profile's reports: a test step must still write
the JUnit report and `{output}/lcov.info`, or the override also replaces
`tasks.test.reports`. An override fails the task with
`stacks.project_config_invalid` when it names an unknown member, changes `id`
or `markers`, or adds a stack-root input the extension does not declare. The
design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## The real-tool proof

The extension's tests run its tasks with a stub cargo. One more test,
`TestTheFixtureRepositoryPassesWithTheRealTools`, runs lint, test and build
for every crate of the observe Cargo detector's fixture repository with the
real cargo, cargo-nextest and cargo-llvm-cov, and checks that every case and
coverage line is attributed. It skips when a tool is missing, unless
`PUTNAMI_RUST_REAL_TOOLS=1`; the workflow `.github/workflows/rust-stack.yml`
installs the tools and runs it on each change to the Rust stack.

## Limits

- A member of a nested Cargo workspace whose root package depends on it is
  left out.
- `build` copies the debug binary only, from the stack root's `target/`.
- No publish: a crate reaches its registry through cargo.
