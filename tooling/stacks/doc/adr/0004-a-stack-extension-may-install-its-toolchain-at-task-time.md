# ADR 0004 — A stack extension may install its toolchain at task time

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and `@putnami/rust`; amends
  [ADR 0003](0003-a-stack-extension-may-provide-its-toolchain.md)

## Context

ADR 0003 lets a stack extension answer `toolchain-resolve`, so the CLI unpacks
one pinned archive. That path does not fit Rust:

- The one archive that holds a whole Rust, `rust-<version>-<triple>.tar.gz`, is
  386 MB, and its components sit in separate directories: `rustc` finds no
  standard library until they are merged.
- The CLI compares a toolchain's version probe with the whole answer.
  `rustc --version` prints `rustc 1.99.1 (58cf8f9c3 2026-10-13)`, so no `expect`
  template can match it without a protocol change.
- A repository's `rust-toolchain.toml` can state any release or a channel. An
  embedded table covers a few releases.

rustup, Rust's own installer, already handles each point: it downloads only the
named components, about 13 s on the machine that tested it, and checks each one
against the channel manifest.

## Decision

A `Stack` may set two functions, and the core calls both before a task's steps.

- **`Provide`** installs the toolchain when the one on `PATH` is missing or fails
  the check. The core then selects it through `toolchain.provisioned`, as it
  selects a pinned one. A machine toolchain that passes the check keeps coming
  first, and nothing is installed.
- **`Tools`** makes the task's other tools available and returns directories
  appended to the task's `PATH`, so a tool the machine holds keeps coming first.

An offline run calls neither. A step that names a program beside the installed
toolchain, such as `cargo` beside `rustc`, runs that program.

`@putnami/rust` uses them as follows:

- `Provide` runs a rustup pinned by SHA-256 per platform, in a rustup home of its
  own under the Putnami home. It installs the stated release, or the extension's
  default when the repository states none, with the `minimal` profile, `rustfmt`,
  `clippy` and `llvm-tools-preview`.
- `Tools` installs cargo-nextest and cargo-llvm-cov for the `test` task, from
  archives pinned by SHA-256 per platform.

## Rejected alternatives

- **A lock pin of the combined archive (ADR 0003).** It needs a new probe form
  and contract rung, merge code for the components, and 386 MB per version.
- **Writing to the machine's `~/.cargo` or `~/.rustup`.** Putnami would change a
  toolchain the developer manages.
- **Setting `CARGO_HOME` for the steps.** Cargo would drop the developer's
  registry cache and `~/.cargo/config.toml`. The installed toolchain sets
  `RUSTUP_HOME` and `RUSTUP_TOOLCHAIN` instead, so a rustup proxy in
  `~/.cargo/bin`, which cargo searches before `PATH`, answers with it.

## Consequences

- `putnami.lock.json` holds no Rust pin. The release comes from the repository's
  `rust-toolchain.toml`, else from the extension's default.
- The first task on a machine downloads, and needs a network. The rustup home
  uses about 600 MB per release.
- rustc still needs a C linker on the machine.
- Targets and components a `rust-toolchain.toml` lists beyond the three above are
  not installed.
- The default release moves only when an extension release changes it.
