# ADR 0001 — An application image holds a static binary

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `@putnami/swift`, the `package` and `publish` commands

## Context

A Swift server application, such as a Vapor or Hummingbird package, compiles
to one executable. `@putnami/go`, `@putnami/typescript` and `@putnami/php`
assemble their images with the OCI assembler of the extension SDK, which needs
no Docker daemon and only copies files onto a base pinned by digest. Nothing
runs inside the image while it is assembled, so the executable is built on the
machine that packages it, which may be macOS or Linux, x86_64 or arm64.

Swift offers two ways to build a Linux executable:

- the Static Linux SDK, which `swift sdk install` adds to a Swift toolchain
  from swift.org, links the executable against musl and the Swift runtime
  statically, from macOS or Linux, for x86_64 or arm64;
- a native Linux toolchain, which links the executable against the Swift
  runtime and Foundation libraries of the machine, so the image needs a base
  that ships the same libraries, such as `swift:<version>-slim`, and the
  machine must run Linux on the image's architecture.

## Decision

`package` builds the executable with the Static Linux SDK of the selected
Swift release, `swift build -c release --swift-sdk <arch>-swift-linux-musl`,
and copies it, with the resource bundles SwiftPM writes beside it, onto
`gcr.io/distroless/static:nonroot` pinned by digest, the base of
`@putnami/go`.

The extension pins the checksum of the Static Linux SDK of each Swift release
it supports. When the SDK of the selected release is missing, `package`
installs it with `swift sdk install <url> --checksum <pin>`. A release with no
pin fails with the releases that have one.

A project may name its own base, pinned by digest. A base with no digest
fails.

## Consequences

- The same command packages the same image on macOS and on Linux, for
  `linux/amd64` and `linux/arm64`, from any of these machines.
- The image holds one file of the build and the resource bundles beside it; it
  has no shell and no package manager, and runs as the user 65532, the
  `nonroot` user of distroless, on any base, listening on `PORT`.
- The Static Linux SDK needs a toolchain from swift.org: the Swift of Xcode
  cannot use it, so a macOS machine packages with a toolchain that swiftly or
  the swift.org installer provides.
- A package that links a C library of the system needs that library in the
  SDK, as musl offers it; a package that cannot link statically names a base
  and builds its own image.
- A new Swift release needs its checksum pinned before its applications
  package.

## Alternatives rejected

- **A native Linux build on `swift:<version>-slim`.** It needs no SDK, but it
  packages only on Linux, for the machine's architecture, and the base carries
  the Swift runtime and Foundation, about 100 MB more than the static base.
- **A generated Dockerfile run with `docker build` in a `swift` image.** It
  builds from any machine, but it requires a Docker daemon, which adds a
  prerequisite the other stacks do not have.
- **The SDK checksum read from swift.org at packaging time.** It needs no pin
  per release, but the build would trust what the network answers instead of
  a reviewed value.
