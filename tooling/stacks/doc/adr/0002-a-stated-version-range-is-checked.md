# ADR 0002 — A stated version range is checked

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `tooling/stacks` (`@putnami/stacks`) and the stack extensions
  built on it, first `@putnami/php`

## Context

The toolchain check of the stack core compares the version on the machine
with the version a toolchain file states. Until now it checked only an exact
version, such as `1.82.0` or a `1.82` channel; any other value got
`stacks.toolchain_unpinned`, a warning, and was not checked at all.

Some stacks state a range, not a version. A PHP repository states the PHP it
runs on in `composer.json`, as `require.php`: `>=8.4`, `^8.2`, `8.3.*`. That
range is the only statement most PHP repositories make, and issue #940 asks
the core to check it. A Dart `environment.sdk` is a range too.

## Decision

A toolchain file may state a range in Composer's constraint syntax:
`>=`, `>`, `<=`, `<`, `!=`, `=`, caret (`^8.2`), tilde (`~8.2`), wildcards
(`8.3.*`, `*`), hyphen ranges (`8.1 - 8.3`), conditions joined by a comma or
a space (and) or by `||` (or). Stability flags such as `@dev` are ignored.

- The machine's version satisfies the range: the task runs, with no
  diagnostic.
- It does not: the task fails with `stacks.toolchain_mismatch`, which names
  the range, the machine's version and the file, unless the project sets
  `toolchain.check` to `warn`.
- A value that is neither an exact version nor a range the core parses, such
  as the Rust channel `stable`, keeps `stacks.toolchain_unpinned`.

The key carries the range as the files state it, never the machine's version.
Only the numeric release of the machine's version is compared: `8.4.0RC1`
satisfies `>=8.4`.

## Rejected alternatives

- **Keep a range unchecked, with the unpinned warning.** A PHP task would run
  on a PHP its repository refuses, and every PHP task of a typical repository
  would warn.
- **Warn on every satisfied range, since two machines share entries.** The
  range is what the repository promises to run on; a team that wants one
  version in the key states it in `.php-version`, which the PHP profile reads
  first.
- **Read Composer's `config.platform.php` as the expected version.** It is
  the version Composer resolves dependencies for, often the lowest the
  repository supports, not the one it runs on: an exact comparison would fail
  most machines.

## Consequences

- Two machines whose versions both satisfy a range share cache entries. A
  repository that needs one version in the key states it exactly.
- A profile lists an exact file first and a range file after it, so an exact
  statement wins: the PHP profile reads `.php-version`, then `composer.json`.
