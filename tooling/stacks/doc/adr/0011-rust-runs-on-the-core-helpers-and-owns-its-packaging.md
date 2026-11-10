# ADR 0011 — Rust runs on the core's helpers and owns its packaging

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and `rust/extension`
  (`@putnami/rust`)

## Context

`@putnami/rust` runs `lint`, `test` and `build` only. `putnami run` and `serve`
start a program that may never end. `package` and `publish` ship artifacts
through channels, and each stack ships different ones.

PHP, Dart and Ruby already serve and run their components with jobs of their
own, on helpers the core shares: `Stack.Selected`, `RunForeground`,
`AnnounceReady` and `FreePort`. No profile declares a workload.

A Rust binary links against the libc of the machine that builds it. A binary
built on Ubuntu 24.04 (glibc 2.39) may not start on the Debian 12 image base
(glibc 2.36), and macOS cannot link a Linux binary with no cross linker.

A released binary may embed any file of its crate, through `include_str!` or a
build script, and the profile's input sets name the sources a task reads, not
the whole crate.

`cargo publish` takes its version from `Cargo.toml` and has no flag to set it,
and it refuses a path dependency with no version.

## Decision

- **`run` and `serve` are jobs of `@putnami/rust`** on the core's helpers, as
  PHP, Dart and Ruby: `cargo run --package <crate> --locked`, with no stack-root
  lock. `serve` sets `PORT` and announces the server ready once its port
  accepts connections. It does not restart on a change.
- **An input set may be a selector.** `Stack.SelectorSets` names input sets a
  manifest reads directly, as `<stack id>:<set>`. `rust:package` is the whole
  crate, less `target/` and the `.gen/version.json` stamp the CLI rewrites in
  every planned project. A walk does not enter a directory an exclusion leaves
  out whole.
- **A root step knows its component.** `RootStep` carries the component's name
  and the version its first toolchain check expects, so a job of the extension
  installs the same Rust the tasks select.
- **Packaging stays in `@putnami/rust`**, as PHP's and Java's stay in their
  extensions. The core gets no `package` or `publish` until a second stack
  needs the same shape.
- **`package` compiles with `cargo zigbuild`**, pinned by SHA-256 per platform
  with zig, and always with the Rust the extension installs, because adding a
  target needs rustup. A Linux target names glibc 2.36.
- **The zig and cargo-zigbuild pins are PyPI wheels.** Their GitHub and
  ziglang.org archives are `.tar.xz`, a format Go's standard library cannot
  read, and a wheel is a zip.
- **`publish --crates` sends the Putnami release version**, written into a copy
  of the stack root that `package --crates` prepares, as Go and npm do. The
  checkout never changes.
- **A dry run names the crate and its path dependencies together**, because
  cargo resolves a dependency that is not yet on the registry only when it is in
  the same invocation.
- **`publish --docker` goes through the shared Docker publisher**, which reads
  `--docker-registry` as [ADR 0005 of the workspace
  protocol](../../../../protocols/workspace/doc/adr/0005-the-docker-registry-flag-overrides-the-oci-registry.md)
  states.

## Rejected alternatives

- **Workload tasks in the profile**, with a core job that runs and watches
  them. The other stacks already serve with jobs of their own on the shared
  helpers; a second mechanism would split them.
- **`cargo build --target`.** It adds no tool, but it links against the
  machine's glibc and needs a cross linker on macOS.
- **`Cargo.toml` as the version source.** A publish would fail until someone
  edits the file for each release.
- **`package` and `publish` in the core.** PHP and Java shipped them first,
  each in its own extension, with an application image through the shared
  Docker publisher. Rust adds crates and release archives, which no other
  stack ships: a shared shape would be guessed from one case.

## Consequences

- `serve` does not restart the program when a source changes.
- A Rust project is not a release-set member: `publish --channel` does not carry
  its crate, image or archives.
- The first `package` of a machine downloads zig, about 100 MB.
- `rust-toolchain.toml` still selects the release, and the compile step installs
  it beside the machine's Rust.
