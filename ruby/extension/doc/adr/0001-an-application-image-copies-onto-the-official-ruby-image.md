# ADR 0001 — An application image copies onto the official Ruby image

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `@putnami/ruby`, the `package` and `publish` commands

## Context

A Rails or Rack application needs Ruby, its bundle and a server such as Puma.
`@putnami/go`, `@putnami/typescript` and `@putnami/php` assemble their images
with the OCI assembler of the extension SDK. That assembler needs no Docker
daemon: it copies files onto a base pinned by digest, and runs nothing inside
the image.

A Ruby bundle is not plain files. Gems such as `puma`, `nio4r` and `nokogiri`
hold native extensions, C code that `bundle install` compiles. The compiled
files load only on the OS, CPU, C library and Ruby version they were built
for. Bundler writes them under
`extensions/<platform>/<api>`, where `<api>` is the Ruby extension API, such
as `3.4.0`, with a `-static` suffix when the Ruby that built them is linked
statically.

## Decision

`package` assembles the image of an application on the official
`ruby:<version>-slim-trixie` image, pinned by digest for each Ruby minor
version the extension ships. It adds three layers:

1. the bundle, installed on the packaging machine with
   `BUNDLE_DEPLOYMENT=1`, `BUNDLE_WITHOUT=development:test` and a private
   `BUNDLE_PATH`, and copied to `/usr/local/bundle`;
2. the application's files, without what a run writes, the tests and the
   secret files;
3. `tmp/` and `log/`, owned by the image's non-root user.

A project may name its own base, pinned by digest. A base with no digest fails
with `ruby.base_not_pinned`.

The packaging machine must be a glibc Linux machine of the image's
architecture, `--platform`, `linux/amd64` by default. The base follows the
minor version of the Ruby that installs the bundle, so the image's Ruby loads
what that Ruby built. Any other machine fails with
`ruby.package_host_mismatch` before it installs anything. The message names the
machine and the target, and says to package on a Linux runner of the target
architecture.

A static Ruby, such as one a version manager built, writes its extensions
under `<api>-static`. The official image's Ruby is shared and reads `<api>`, so
`package` renames those directories.

## Consequences

- A Mac, or an amd64 runner for an arm64 image, packages nothing. A team
  packages in CI, on a Linux runner of each target architecture.
- The slim image has no compiler and no database client library. An
  application whose gems load one at run time, such as `pg` with `libpq`,
  names its own base.
- `bundle install` runs gem build scripts on the packaging machine, as it does
  on a developer's machine. Nothing runs inside the image.
- `publish` shares the publisher of the other stacks, with its registry and
  release-set rules.

## Alternatives rejected

- **Build inside a container engine.** `package` would run `bundle install` in
  a container of the base image, of the target platform, so any machine could
  package for any target. It requires a Docker daemon, or an emulator for a
  foreign CPU, which adds a prerequisite the other stacks do not have.
- **Unpack platform gems for a foreign target.** Many native gems publish
  prebuilt gems per platform, such as `nokogiri-1.18.0-aarch64-linux-gnu`.
  `package` could download those for the target and skip compiling. A gem
  without a prebuilt build for the target, such as `puma`, would still need a
  compiler for that target, so this covers part of a bundle only. It may come
  as a follow-up for bundles that it covers whole.
