# ADR 0003 — A stack extension may provide its toolchain

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the stack extensions
  built on it, first `@putnami/php`

## Context

The stack core compares the toolchain on the machine with the version a file
states, and never installs one. A team that adds `@putnami/php` needs PHP,
Composer and a coverage driver before its first task. Go, Bun and uv have no
such prerequisite: the extension answers `toolchain-resolve` and the CLI
installs the pinned archive.

PHP publishes official binaries for Windows only. For Linux and macOS the
available builds are those of the static-php-cli project, on
`dl.static-php.dev`, and they hold neither PCOV nor Xdebug.

## Decision

A stack extension may answer `toolchain-resolve` for its toolchain, and declare
it as an optional runtime toolchain.

- The toolchain on `PATH` comes first when it satisfies the stated version or
  range, because it may hold a coverage driver or the extensions a project
  needs. Otherwise the task puts the pinned toolchain first on `PATH` and
  repeats the check.
- `@putnami/php` pins PHP 8.3, 8.4 and 8.5 by SHA-256: `dl.static-php.dev` on
  Linux and macOS, `windows.php.net` on Windows. Composer is a pinned `.phar`
  the extension downloads itself, since a lock archive is a `tar.gz` or a
  `zip`.
- A profile may name how to run a test without coverage. On a toolchain with
  no coverage driver, the task runs those steps and warns with
  `stacks.coverage_unavailable`.

## Rejected alternatives

- **Build and publish PHP archives with a coverage driver.** Every PHP
  security release would become Putnami's to build, for a stack that is not a
  Putnami priority.
- **An exact pin, as Go, Bun and uv.** A machine PHP of another patch release
  would be passed over, and its coverage driver with it.
- **Keep PHP a prerequisite.** A team could not try the extension on a machine
  with no PHP.

## Consequences

- The Linux and macOS archives are third-party binaries. Their digests are
  shipped with the extension, so a changed archive fails its download.
- A pin moves only when an extension release ships new digests.
- A cached test result of a machine with no driver holds no coverage, and a
  machine with a driver can reuse it: the key carries no version or path of the machine, only whether the toolchain was available.
