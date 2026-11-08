# ADR 0002 — An application image installs wheels onto the official Python image

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/python`, the `package` and `publish` commands

## Context

`@putnami/go`, `@putnami/typescript` and `@putnami/php` package an application
as an OCI image with the assembler of the extension SDK. The assembler needs no
Docker daemon: it copies files onto a base pinned by digest and runs nothing
inside the image. A Python application needs an interpreter in the image and
its locked production dependencies, built for the image's platform rather than
for the machine that packages it.

## Decision

`package --docker` assembles the image of an application on the official
`python:<version>-slim` image, pinned by digest for each Python minor the
extension ships. The minor is that of the interpreter `uv python find` selects
at the workspace root, the one the tests run on. A project may name its own
base, pinned by digest. A base with no digest fails.

The dependencies come from `uv.lock`. `uv export --frozen --no-dev` lists the
production dependencies of the project. `uv pip install --target` installs the
registry ones as wheels only, checked against their locked hashes, for the
image's platform and Python minor. It builds nothing, so a `linux/arm64` image
can be packaged on an `amd64` machine. The project and its workspace path
dependencies are copied to a staging directory and built into wheels there.
The installed packages go to one directory that `PYTHONPATH` names.

The image runs `python /app/<entrypoint>`, the entrypoint `serve` runs, as an
unprivileged user, with `PORT` set to the port it exposes.

`publish --docker` pushes the image through the shared publisher, and the
workspace probe declares the image as an `oci` release-set member.

## Consequences

- A registry dependency that publishes no wheel for the image's platform fails
  packaging, naming the dependency. Nothing is compiled during assembly.
- A workspace path dependency with native code is built for the machine that
  packages it.
- A named base must provide `python` of the same minor on its `PATH`, with a
  glibc that runs `manylinux_2_28` wheels.
- The application files are read-only for the user that runs them, and
  Python writes no bytecode at run time.

## Alternatives rejected

- **Copy the virtual environment.** A virtual environment points at the
  interpreter of the machine that created it, and its wheels are built for
  that machine.
- **A generated Dockerfile run with `docker build`.** It requires a Docker
  daemon, which adds a prerequisite.
- **A distroless Python base.** No official image is published for each Python
  minor.
