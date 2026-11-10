# ADR 0001 — Cargo runs offline from the home workspace-fetch fills

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/rust`: `workspace-fetch`, `workspace-install`,
  `deps-upgrade`, and every job of a run whose dependencies are offline

## Context

A hosted run downloads the dependencies with `workspace-fetch`, the one job
with the network and the read credential, before any repository process.
Every later job runs with `PUTNAMI_OFFLINE_DEPENDENCIES=1` and must not reach
the network for them. The TypeScript, Go, Ruby and PHP extensions fetch into a
cache their installers and tasks read.

`cargo fetch --locked` downloads the crates and git dependencies a
`Cargo.lock` names into cargo's home, and builds nothing. It still asks rustc
for the targets it knows, through the `build.rustc`, `build.rustc-wrapper` and
`build.rustc-workspace-wrapper` that the repository's `.cargo/config.toml` may
name. cargo reads `CARGO_NET_OFFLINE`, its `net.offline` setting, in every
command, the profile's steps included. The stack core starts those steps with
the environment of the runtime's process, and has no hook to add a variable
to them.

The read credential's hosts are the Putnami registries, and none of them
serves a Cargo registry. cargo authenticates to an alternative registry with
its own credential providers.

`cargo update --breaking` is unstable in the cargo of Rust 1.99.1, the release
the extension installs by default.

## Decision

- **`workspace-fetch` runs `cargo fetch --locked`** at each stack root that
  holds a `Cargo.lock`, with `RUSTC_WRAPPER` and `RUSTC_WORKSPACE_WRAPPER`
  empty and `RUSTC` set to the selected rustc. A root with no `Cargo.lock` is
  left out with a warning: the fetch writes nothing in the repository.
- **The fetch reads and closes the credential descriptor first, and gives
  cargo nothing.**
- **Offline, each job of the runtime but `publish-crates` sets
  `CARGO_NET_OFFLINE=true` in its own process** before it starts cargo, so
  every cargo it starts inherits it. `cargo publish` uploads, so it keeps the
  network.
- **No job sets `CARGO_HOME`**, as ADR 0004 of the stack core states: the
  fetch and the tasks read the environment's cargo home.
- **`workspace-install` runs `cargo fetch`**, which writes a missing
  `Cargo.lock`, and offline `cargo fetch --locked --offline`, which checks the
  lockfile against cargo's home and writes nothing. It also installs the test
  tools ahead of the first test.
- **`deps-upgrade` runs `cargo update`** within the semver requirements, as
  `bundle update` and `composer update` do, and fails offline.

## Rejected alternatives

- **`--offline` appended to each step.** A step override would drop it, and
  the arguments would move every task's command line.
- **A cargo home of the extension's own, under its machine cache.** cargo would
  drop the developer's registry cache and `~/.cargo/config.toml`, the reason
  ADR 0004 of the stack core gives.
- **Writing the read credential into a cargo credential.** No Cargo registry
  accepts it.
- **`cargo update --breaking` or cargo-edit's `cargo upgrade`.** The first
  needs a nightly cargo, the second one more tool to install.

## Consequences

- A hosted run tests from the crates `workspace-fetch` downloaded, and a crate
  cargo's home lacks fails the step rather than reaching the network.
- An offline task runs the Rust and the test tools on `PATH`: the stack core
  installs and selects the ones under the Putnami home only with the network.
- A credential provider the repository's Cargo configuration names still runs
  when cargo needs a token for a registry that requires one.
