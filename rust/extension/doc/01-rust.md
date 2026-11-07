# Run a Cargo workspace with Putnami

`@putnami/rust` turns each crate of a Cargo workspace into a Putnami project
and runs its lint, test and build with the cargo tools the team already uses.
It reads the repository the way `putnami observe` does, and it never installs
a toolchain.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/rust"] }
   ```

2. Run `putnami projects sync`. Each directory that holds a `Cargo.toml` with
   a `[package]` table, or that the root `[workspace]` names as a member,
   joins the workspace's `includes`. No `putnami.json` is needed: the
   extension names the crate by its package, adds an edge to each crate it
   depends on by path, and tags it `rust`.
3. Run `putnami lint,test,build`.

## What each task runs

Each task runs in the crate's directory, with no shell. `{component}` is the
crate's package name.

| Task | Steps |
|---|---|
| `lint` | `cargo fmt --package {component} --check`, then `cargo clippy --package {component} --all-targets -- -D warnings` |
| `test` | `cargo llvm-cov nextest --package {component} --profile ci --lcov --output-path {output}/lcov.info` |
| `build` | `cargo build --package {component} --locked` |

The test task needs cargo-nextest and cargo-llvm-cov, and a nextest profile
`ci` that writes a JUnit report, in `.config/nextest.toml` at the workspace
root:

```toml
[profile.ci.junit]
path = "junit.xml"
```

It then reads `target/nextest/ci/junit.xml` and the LCOV report into the test
and coverage summary. A failed case, a step that exits non-zero, or a missing
report fails the task. A report left at the root by another crate or an
earlier run is removed before the steps, so it is never read as this crate's.
A task of a crate waits for the same task of the crates it depends on, and
the crates of one Cargo workspace run one task at a time,
because they share its `target/` directory.

## The toolchain check

Before its steps, each task compares `rustc --version` with the version that
`rust-toolchain.toml` (`toolchain.channel`) or `rust-toolchain` states, in the
crate's directory, then at the workspace root.

| Case | Result |
|---|---|
| The versions match | The task runs |
| They differ | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| `rustc` is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| No file states an exact version, such as `channel = "stable"` | The warning `stacks.toolchain_unpinned`: the cache key carries no toolchain version |

## What reaches the cache key

A task's key covers the crate's files its task reads, the workspace-root
files (`Cargo.toml`, `Cargo.lock` for test and build, `.cargo/config.toml`,
`.config/nextest.toml`, the rustfmt and clippy configurations, the toolchain
files), the crate's
`putnami.json`, the expected toolchain version, and the key of the same task
of each crate it depends on. `lint` does not read `Cargo.lock`, so a lockfile
change reruns test and build only.

| Input set | Patterns |
|---|---|
| `sources` | `src/**`, `tests/**`, `benches/**`, `examples/**`, `build.rs` |
| `manifests` | `Cargo.toml`, `.cargo/config.toml`, `rustfmt.toml`, `.rustfmt.toml`, `clippy.toml`, `.clippy.toml`, in the crate and at the workspace root, and `.config/nextest.toml` at the root |
| `lockfiles` | `Cargo.lock`, and the workspace root's `Cargo.lock` |

`lint` reads `sources` and `manifests`; `test` and `build` read all three.

## Override the profile

A crate overrides any value of the profile in its own `putnami.json`, under
`options["@putnami/rust"]`. An object merges; any other value, such as a list
of steps or patterns, replaces the profile's whole.

```json
{
  "options": {
    "@putnami/rust": {
      "tasks": {
        "test": { "steps": [["cargo", "llvm-cov", "nextest", "--package", "{component}", "--profile", "ci", "--features", "slow", "--lcov", "--output-path", "{output}/lcov.info"]] }
      },
      "inputs": { "sources": ["src/**", "tests/**", "fixtures/**"] },
      "toolchain": { "check": "warn" }
    }
  }
}
```

A steps override keeps the profile's reports: a test step must still write
the JUnit report and `{output}/lcov.info`, or the override also replaces
`tasks.test.reports`. An override fails the task with `stacks.project_config_invalid` when it names
an unknown member, changes `id` or `markers`, or adds a workspace-root input
the extension does not declare. The design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- A Cargo workspace whose root is not the Putnami workspace root: its crates
  are read as their own stack roots, so the test task looks for the JUnit
  report under each crate's `target/` and fails with `stacks.report_missing`,
  and the workspace's `Cargo.lock` reaches no key (#939).
- Each nextest case and coverage line is not yet mapped to its crate (#939).
- No publish: a crate reaches its registry through cargo.
