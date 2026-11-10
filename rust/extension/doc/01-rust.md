# @putnami/rust reference

`@putnami/rust` turns each crate of a Rust repository into a Putnami project
and runs its lint, test and build with the cargo tools the team already uses.
It reads the repository the way `putnami observe` does, needs no
`putnami.json` in a typical repository, and installs Rust and its test tools
when the machine lacks them.

## Adopt it

`@putnami/rust` is not published yet: until its first release, `putnami init
--extension rust` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

1. At the root of the repository, run:

   ```bash
   putnami init --extension rust
   ```

   It writes `putnami.workspace.json` with `@putnami/rust` declared, installs
   the extension, and runs `putnami projects sync`, because the repository
   holds a `Cargo.toml`. A repository of several languages takes a list, such as
   `--extension rust,typescript`, and `putnami init` without `--extension`
   proposes the languages it finds. In a workspace that already exists,
   declare the extension in `putnami.workspace.json` and run
   `putnami projects sync`:

   ```json
   { "extensions": ["@putnami/rust"] }
   ```

2. Each directory that holds a `Cargo.toml` with
   a `[package]` table, or that a `[workspace]` names as a member, joins the
   workspace's `includes`, and so does each virtual Cargo workspace below the
   workspace root. The extension names each crate by its package, adds an edge
   to each crate it depends on by path, and tags it `rust`.
3. Give the nextest profile `ci` a JUnit report, in `.config/nextest.toml` at
   the Cargo workspace root:

   ```toml
   [profile.ci.junit]
   path = "junit.xml"
   ```

4. Run `putnami lint,test,build`. A second run over an unchanged tree is a
   cache hit for every crate.

## What it installs

| Tool | When | From | Where |
|---|---|---|---|
| Rust, with rustfmt, clippy and llvm-tools | `rustc` is missing, or outside the version a `rust-toolchain` file states | A rustup pinned by SHA-256 | `<Putnami home>/tools/rust/rustup-home` |
| cargo-nextest, cargo-llvm-cov | The `test` task, or `putnami install` ahead of it, when `PATH` holds neither | Archives pinned by SHA-256 | `<Putnami home>/tools/rust` |

A Rust on `PATH` that satisfies the statement always runs. With no statement,
the extension installs its default release, 1.99.1. Your `~/.cargo` and
`~/.rustup` stay untouched, and `putnami.lock.json` holds no Rust pin.

An offline run installs nothing and fails with `stacks.toolchain_missing`.
rustc needs a C linker on the machine: `cc` on Linux and macOS, the MSVC build
tools on Windows. A Rust with no llvm-tools, such as a Homebrew one, runs
`cargo nextest run`: `test` warns with `stacks.coverage_unavailable` and reads no
coverage report. Targets and components a `rust-toolchain` file lists beyond
those three are not installed.

## Install, fetch and upgrade the crates

The crates live in cargo's home, `CARGO_HOME` when it is set, else
`~/.cargo`, as with any cargo command. No job of the extension sets
`CARGO_HOME`, so every job reads the same cargo home.

| Command | What it runs |
|---|---|
| `putnami install` | `workspace-install`: `cargo fetch` at each stack root of the workspace's projects, except the projects `disable.tags` excludes, which writes a `Cargo.lock` a root lacks. A root where `cargo fetch --locked --offline` passes, whose crates cargo's home already holds, is current and skipped; `--force` fetches it again. It then installs cargo-nextest and cargo-llvm-cov when `PATH` holds neither, so the first test does not wait for them; a failed download is the warning `rust.test_tools_unavailable`. |
| `putnami workspace-fetch` | `cargo fetch --locked` at each stack root that holds a `Cargo.lock`. A hosted run calls it with the network, before any repository process. |
| `putnami deps-upgrade` | `cargo update` at each stack root, with the cargo the toolchain check selects. |

`cargo fetch` downloads the crates and git dependencies that `Cargo.lock`
names and runs none of their code: no build script and no procedural macro.
`workspace-fetch` writes nothing in the repository, so a stack root with no
`Cargo.lock` is left out with the warning `rust.lockfile_missing`, which names
it: run `putnami install` and commit the lockfile. The fetch also runs with
`RUSTC_WRAPPER` and `RUSTC_WORKSPACE_WRAPPER` empty and `RUSTC` set to the
selected rustc, which cargo asks for the targets it knows, so no wrapper or
compiler the repository's `.cargo/config.toml` names runs.

A hosted run hands `workspace-fetch` a read credential on a descriptor. Its
hosts are the Putnami registries, and none of them serves a Cargo registry:
the job reads and closes the descriptor before it starts any process and gives
cargo nothing. cargo authenticates to an alternative registry with its own
credential providers, `CARGO_REGISTRIES_<NAME>_TOKEN` or the cargo home's
`credentials.toml`.

`cargo update` moves each locked version to the newest one its semver
requirement in `Cargo.toml` accepts, as `bundle update`, `composer update`,
`pub upgrade` and `swift package update` do. It bumps no major version:
`cargo update --breaking` is unstable in the cargo of Rust 1.99.1, the release
the extension installs by default. To move to a new major version, edit the
requirement in `Cargo.toml`, then run `putnami install`.

`Cargo.lock` at the workspace root is an install input of the workspace: when
it changes, the next Putnami command installs again before it runs.

## Offline runs

When `PUTNAMI_OFFLINE_DEPENDENCIES` is `1`, as in every job of a hosted run
but `workspace-fetch`, each job of the extension sets `CARGO_NET_OFFLINE=true`
before it starts cargo. `lint`, `test`, `build`, `run`, `serve`, `package` and
`workspace-install` then read only the crates `workspace-fetch` downloaded, and
a crate cargo's home lacks fails the step rather than reaching the network.

| Job | Offline |
|---|---|
| `workspace-install` | `cargo fetch --locked --offline` at each root that holds a `Cargo.lock`: it checks the lockfile against cargo's home and writes nothing. A root with no `Cargo.lock` is left out with `rust.lockfile_missing`. It installs no tool. |
| `deps-upgrade` | Fails with `rust.cargo_update_failed`: `cargo update` needs the network. |
| `publish --crates` | Keeps the network: `cargo publish` uploads to its registry. |

An offline task installs no Rust and no test tool: a hosted run runs the
`rustc`, cargo-nextest and cargo-llvm-cov on its runner's `PATH`. The choices
behind these jobs are in
[ADR 0001](https://github.com/Putnami/putnami/blob/main/rust/extension/doc/adr/0001-cargo-runs-offline-from-the-fetched-home.md).

## Crates and stack roots

A crate's **stack root** is the directory of the Cargo workspace that holds
it: its `Cargo.toml`, `Cargo.lock`, `.cargo/config.toml` and `target/`.

| Crate | Stack root |
|---|---|
| A member of the workspace root's `[workspace]`, by a `members` glob or as a path dependency of a member below it, or the root package | The workspace root |
| A member of a nested `[workspace]`: the one its `package.workspace` names, else the nearest one above it that names it, past one that excludes it | That workspace's directory |
| A crate that declares its own `[workspace]`, or that no workspace names | Its own directory |

A virtual Cargo workspace below the workspace root, such as
`services/billing` with the members `services/billing/core` and
`services/billing/http`, is an **input-only project**: its tasks run no step,
and each task reads the root files it needs. Each of its crates depends on
it, so a change to `services/billing/Cargo.lock` reruns their test and build,
and not their lint. A member whose stack root reaches it over the crates'
edges, such as a member its root package depends on, is left out with
`stacks.stack_root_unsupported`: the edge to its root would close a cycle.

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

`lint` also checks every relative link and anchor of the crate's `README.md`
files and `doc/` tree, with the documentation link check every Putnami
extension shares: a broken one fails lint, and `--docs-links=false` turns the
check off. A crate below the project, marked by its `Cargo.toml`, answers for
its own documents. The check needs no Rust.

Before its steps, `build` removes `{root}/target/debug/{component}`, and after
them copies the crate's binary from there into its output when cargo wrote
one, so a cache hit restores it and an older build's binary is never kept. A
library crate, or a target directory moved by `CARGO_TARGET_DIR` or
`build.target-dir`, copies nothing. The copy is a debug binary, stored in the
cache with each build result.

## Run and serve a crate

Both run `cargo run --package <crate> --locked -- <args>` in the crate's
directory, after the toolchain check, with no cache and no stack-root lock:
cargo locks its own build directory. They are jobs of `@putnami/rust`, not
profile tasks, so a crate's `putnami.json` does not override them.

`putnami run` starts a binary crate once. `--args` reaches the program,
`--port` sets `PORT`, `--entrypoint` names one of the crate's binaries (`cargo
run --bin <name>`), and the command exits with the exit code of the program.
`putnami serve` starts it, with the same `--args` and `--entrypoint`, and with
`PORT` set from `--port`, 3000 by default and a free port for 0, and runs it
until you stop it. It announces the server ready
once the port accepts connections. A program that exits on its own fails the
serve with its exit code. `serve` does not restart the program when a source
changes.

A crate is an application when it has a `[[bin]]` table, a `src/main.rs` or a
target under `src/bin`, unless `package.autobins` is false. `run` and `serve`
skip a library.

## Package a crate

`putnami package` runs on an application and compiles nothing for a library.
Each channel has a flag, and the project's `publish` field adds to them.

| Flag | Output, under `.putnami/out/<project>/package` |
|---|---|
| `--archives` | `archives/<artifact>-<platform>.tar.gz`, the binary at the root, and `metadata.json` |
| `--docker` | `docker/oci`, an OCI layout, and `docker/manifest.json` |
| `--crates` | `crates/workspace`, a copy at the release version |

The compile step runs `cargo zigbuild --release` for each platform the channels
need: `--platform` for the image, `linux/amd64` by default, and `--platforms`
for archives, the five archive platforms by default. A Linux binary links
against glibc 2.36, the libc of the image base. A crate with several binaries
states `package.default-run`.

The first `package` of a machine installs the pinned zig, about 100 MB, and
`cargo-zigbuild` under the Putnami home, with the Rust the project states and
the targets it needs. An offline run fails and names what is missing.

The image holds the binary at `/app` on a digest-pinned `cc-debian12` base, with
no docker daemon. `--port` sets `PORT` and the exposed port, 8080 by default,
`--entrypoint` names the binary among several, `--docker-tag` adds a tag,
`--docker-base-image` replaces the base and `--docker-load` loads the image
into the local daemon. `--dry-run` plans the archives and the image and builds
neither; a crate is still staged. Every flag of `package` and `publish` is
also read in its camelCase form, such as `dockerTag`, in `putnami.json`. The image name is the project name in lowercase;
an archive keeps the project name as it is written.

`--crates` copies the stack root without `target`, `.git`, `.putnami` and
`node_modules`, with each path dependency outside it. It writes the release
version into each manifest and an exact version on each normal path dependency.
The checkout does not change.

## Publish a crate

`putnami publish` runs `package` first.

| Flag | Effect |
|---|---|
| `--crates` | `cargo publish` from the copy, dependencies first. A version the registry already holds is reused. `--crates-registry` names a registry of the Cargo configuration |
| `--docker` | Pushes the image through the shared Docker publisher. `--docker-registry` names the registry |

`--dry-run` uploads nothing. It names the crate and its path dependencies in one
`cargo publish --dry-run`, with the Rust the extension pins, because cargo takes
several crates from 1.90. Cargo reads the token from `CARGO_REGISTRY_TOKEN` or
its own configuration.

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

## Filter the tests

`putnami test --test <filter>`, or `-t <filter>`, runs the tests whose name
contains the filter. The test step passes it to nextest as
`-E 'test(~<filter>)'`, with the characters a nextest name matcher reads only
escaped, so a filter such as `tests::lines` or `-x` is one name and never an
option. The filter takes that form with coverage, where `cargo llvm-cov
nextest` hands it to `cargo nextest run`, and on a Rust with no llvm-tools,
where the step is `cargo nextest run` itself.

Putnami passes the filter to the crates the selected ones depend on too, where
it can select no test. The step adds `--no-tests=pass` when
`cargo nextest run --help` lists `--no-tests`, as the pinned 0.9.151 does, so
such a crate passes with no case. The filter is part of the task's key: a
filtered result never answers an unfiltered run. A test step override that
runs no nextest gets no filter.

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
| `package` | `**/*`, less `target/` and every `.gen/version.json` stamp |

The compile step of `package` reads the `package` set as the `rust:package`
selector: a released binary may embed any file of the crate. The CLI rewrites
the `.gen/version.json` stamp in every planned project, so its build time would
move the key on every run.

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
[its design note](https://github.com/Putnami/putnami/blob/main/tooling/stacks/doc/01-design.md).

## The real-tool proof

The extension's tests run its tasks with a stub cargo. One more test,
`TestTheFixtureRepositoryPassesWithTheRealTools`, runs `workspace-fetch` into
an empty cargo home on the observe Cargo detector's fixture repository, whose
`pricing` crate depends on `itoa` from crates.io. It then runs lint, test and
build for every crate offline, with the real cargo, cargo-nextest and
cargo-llvm-cov, and checks that every case and coverage line is attributed.
Last, it runs `test -t lines`, with and without coverage: the filter selects
one case in `pricing` and in `api`, and `worker`, where it selects none,
passes. It skips when a tool is missing, unless
`PUTNAMI_RUST_REAL_TOOLS=1`; the workflow `.github/workflows/rust-stack.yml`
installs the tools and runs it on each change to the Rust stack.

## Limits

- A member of a nested Cargo workspace whose root package depends on it is
  left out.
- `build` copies the debug binary only, from the stack root's `target/`.
- A Rust project is no release-set member: `publish --channel` carries neither
  its crate, nor its image, nor its archives.
- The dry run of a crate with a path dependency outside its Cargo workspace
  checks the crate's file list only: cargo cannot build against a dependency
  that is not on the registry yet. The diagnostic names it.
- `deps-upgrade` stays within each semver requirement: it bumps no major
  version.
- An offline task runs the Rust and the test tools on `PATH`, not the ones an
  earlier run installed under the Putnami home.
