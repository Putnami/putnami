# How `putnami observe` reads a Rust repository

The Cargo detector finds the crates of a repository from its `Cargo.toml`
files. It reads files only: it runs no `cargo`, `cargo metadata`,
cargo-nextest or cargo-llvm-cov. The change analyses use what it finds to
name the changed and dependent crates, attribute tests and coverage, and diff
the `Cargo.lock` versions.

## Components

Each crate is a component of ecosystem `rust`:

- When the root `Cargo.toml` has a `[workspace]` table, each directory its
  `members` globs match, less those at or below an `exclude` entry, that holds a
  `Cargo.toml` is a component of source `workspace`. A glob segment is `*`,
  `?` or `[…]`, or `**` for any number of directories. The root is one too
  when it has a `[package]` table; a virtual manifest, with no `[package]`, is
  none.
- Any other `Cargo.toml` with a `[package]` table is a component of source
  `manifest`. A `Cargo.toml` with neither, such as one that only sets
  `[profile]` options, is none.

Only the root's workspace is read: a `[workspace]` in a subdirectory makes its
crates manifest components, and only their `path` dependencies give edges, not
their `workspace = true` ones. Cargo also makes a member of each path dependency
of a member that lies below the workspace root; the detector does not, so
such a crate, outside the `members` globs, is a `manifest` component, with
the same edges.

The search skips `target`, `node_modules`, `vendor` and the directories whose
name starts with `.`, such as `.git` and `.cargo`. A component's id is its
directory relative to the repository root, `.` for the root, and its name the
`[package]` name. A file belongs to the innermost component whose directory
holds it, so the `Cargo.lock` of a virtual workspace root belongs to no
component.

## Dependency edges

A crate depends on another crate of the repository through an entry of its
`[dependencies]`, `[dev-dependencies]` or `[build-dependencies]`, or of the
same tables under a `[target.<platform>]` table, when:

- the entry has a `path`, such as `{ path = "../pricing" }`, resolved against
  the crate's directory;
- the crate is a workspace member, the entry has `workspace = true` and the
  root's `[workspace.dependencies]` entry of the same key has a `path`,
  resolved against the root.

A registry or `git` dependency is no edge, nor is a path outside the
repository or to a directory that is no component. The `[patch]` and
`[replace]` tables are not read.

For example, in this workspace `api` and `worker` depend on `pricing`:

```toml
# Cargo.toml
[workspace]
members = ["crates/*"]

[workspace.dependencies]
pricing = { path = "crates/pricing" }
```

```toml
# crates/api/Cargo.toml
[package]
name = "api"

[dependencies]
pricing = { workspace = true }
```

```toml
# crates/worker/Cargo.toml
[package]
name = "worker"

[dependencies]
pricing = { path = "../pricing" }
```

A change to `crates/pricing` marks `crates/api` and `crates/worker`
dependent.

## Dependencies

A changed `Cargo.lock` is read at the merge base and at the head. Each
`[[package]]` with a `source`, a registry or a `git` one, is a dependency of
ecosystem `cargo`; a package with no `source` is a crate of the workspace or a
path dependency, and is left out. A crate locked at two versions, such as
`syn` 1 and 2, has both. A `pkg:cargo/…` package URL of an SBOM matches the
change.

## Reports

Give cargo-nextest a profile that writes a JUnit report, in
`.config/nextest.toml`:

```toml
[profile.ci.junit]
path = "junit.xml"
```

Then run the tests with cargo-llvm-cov, which runs nextest and writes the
coverage report:

```bash
cargo llvm-cov nextest --profile ci --workspace --lcov --output-path lcov.info
```

nextest writes `target/nextest/ci/junit.xml`; `--cobertura --output-path
cobertura.xml` writes Cobertura instead of LCOV. Report discovery recognizes a
Cargo target directory by the `CACHEDIR.TAG` cargo writes in it, and walks in
it only its `nextest` store, the target directories nested in it, such as the
`llvm-cov-target` cargo-llvm-cov builds in, and the files it holds directly:
the build outputs, which can hold more entries than the discovery bound, are
not read. A target directory that `CARGO_TARGET_DIR` moves outside the
repository is not walked: name its report with `--junit`.

nextest names each case's class and suite with its test binary id: the package
name for the unit tests of a library, then `::` and the target for the other
binaries, such as `pricing::integration` for `tests/integration.rs` or
`api::bin/api`. The case goes to the crate of that package name, `-` and `_`
read as one. cargo-llvm-cov writes the absolute path of each source file,
which maps from any checkout directory. See
[tests and coverage](/docs/tooling-&-workspace/observe/change-analyses#tests-and-coverage).

## Diagnostics

| Code | Meaning |
|---|---|
| `observe.cargo_manifest_invalid` | A `Cargo.toml` cannot be parsed: that file is no component. Or a `members` or `exclude` pattern of the workspace is not a repository-relative glob: it is ignored. |
| `observe.cargo_walk_bounded` | The search for `Cargo.toml`, or for the workspace members, stopped after 200,000 entries or could not walk the repository. |
