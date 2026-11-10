# ADR 0002 — An application image holds an AOT executable on a glibc base

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/dart`, the `package` and `publish` commands

## Context

A Dart server, such as a `shelf` or `dart:io` application, runs from an
entrypoint under `bin/`. The `build` task already compiles each `bin/*.dart`
with `dart compile exe` for the machine that runs it. `@putnami/go`,
`@putnami/rust` and `@putnami/swift` assemble their images with the OCI
assembler of the extension SDK, which needs no Docker daemon and only copies
files onto a base pinned by digest: nothing runs inside the image while it is
assembled, so the executable is built on the machine that packages it, which
may be macOS, Linux or Windows, x86_64 or arm64.

`dart compile exe` writes a self-contained executable: the ahead-of-time
snapshot of the program and the Dart runtime in one file. From Dart 3.8.0 it
cross-compiles to Linux with `--target-os linux --target-arch x64|arm64` from
any 64-bit host; Dart 3.7 has `--target-os` but no `--target-arch`. A target
other than the host's downloads its `gen_snapshot` and `dartaotruntime` from
the dart-archive into `~/.dart` on first use.

The executable is not static. On Linux x64 and arm64 it needs the C library
and nothing else: `readelf -d` of a 3.13.7 executable lists `libc.so.6`,
`libm.so.6`, `libdl.so.2`, `libpthread.so.0` and the dynamic loader, with
symbols up to `GLIBC_2.18`, and no C++ runtime. TLS goes through the BoringSSL
the runtime links, which reads the CA certificates under `/etc/ssl/certs`.

A Flutter package builds an app for a store, a desktop or the web, not a
server, and a package with build hooks needs `dart build cli`, which
`dart compile` refuses, and the native libraries it bundles beside the
executable.

## Decision

An application is a pure Dart package, of the `dart` or `dart-in-flutter`
variant, with a `bin/*.dart`. Its image holds one executable.

`package` compiles the entrypoint with the selected `dart`, the one `build`
runs, as `dart compile exe <entrypoint> --target-os linux --target-arch
<x64|arm64>`, holding the lock of the stack root, and copies the executable to
`/app/<name>` onto `gcr.io/distroless/cc-debian12:nonroot` pinned by digest,
the base of `@putnami/rust`: Debian 12's glibc 2.36, CA certificates, time
zones, no shell.

The entrypoint is the one `serve` starts: the file or `bin/` executable
`entrypoint` names, a project naming it in `options.package.entrypoint`, else
`bin/server.dart`, else `bin/<package>.dart`, the one `dart run` starts, else
the package's only `bin/*.dart`. Several with none of these fail and name them.

A Dart below 3.8.0 fails with `dart.cross_compile_unsupported`, which names
the version and the constraint to state. `--platform` takes `linux/amd64`, the
default, or `linux/arm64`; another fails with `dart.platform_unsupported`.

The image runs as the user `65532:65532`, the `nonroot` user of distroless, by
number so that any base runs it unprivileged, in `/app`, with `PORT=8080` set
and exposed, as the images of `@putnami/swift`, `@putnami/java` and
`@putnami/dotnet` do. A project may name its own base, pinned by digest; a
base with no digest fails with `dart.base_not_pinned`.

## Consequences

- The same command packages the same image on macOS, Linux and Windows, for
  `linux/amd64` and `linux/arm64`, with no Docker daemon and no Linux machine.
- The image holds one file of the build: no `pubspec.yaml`, no `.dart_tool`
  and no asset. A server that reads files at run time, such as a `public/`
  directory, names a base that holds them.
- A repository that packages states `environment.sdk` from 3.8.0, so that the
  SDK its tasks install can cross-compile.
- An arm64 image packaged on an x64 machine, or the reverse, downloads the
  target's tools once per SDK version: an offline run needs them in `~/.dart`
  already.
- A program that loads a native library through `dart:ffi` finds only what the
  base holds: glibc and the C++ runtime of `cc-debian12`. It names a base with
  its library.

## Alternatives rejected

- **`gcr.io/distroless/static`, the base of `@putnami/go` and
  `@putnami/swift`.** It has no C library, and `dart compile exe` cannot link
  one statically: the executable does not start.
- **`gcr.io/distroless/base-debian12`.** It holds the glibc the executable
  needs and not the C++ runtime, a few megabytes less, but it is a second
  distroless pin to maintain, and a `dart:ffi` library more often needs
  `libstdc++` than not.
- **`dart compile exe` for the host only, the executable `build` writes.** It
  needs no cross-compilation, but it packages only on Linux, for the machine's
  architecture, and a macOS machine could not package at all.
- **A generated Dockerfile run with `docker build` in a `dart` image.** It
  builds from any machine, but it requires a Docker daemon, which no other
  stack needs to package.
- **`dart build cli`.** It writes a bundle, the executable under `bin/` and the
  dynamic libraries of build hooks under `lib/`, and cross-compiles with the
  same flags in recent releases, but Dart 3.8 has no `dart build`, and a
  hook's library needs the system libraries it links, which no single base
  answers. Packages with build hooks stay out; a later decision may package
  the bundle.
