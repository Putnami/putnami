# ADR 0001 — An application image copies onto the official PHP image

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/php`, the `package` and `publish` commands

## Context

A Symfony application needs an HTTP server and the PHP it runs on. `@putnami/go`
and `@putnami/typescript` assemble their images with the OCI assembler of the
extension SDK, which needs no Docker daemon and only copies files onto a base
pinned by digest. Putnami builds and publishes no PHP archive and no base image.

## Decision

`package` assembles the image of an application on the official
`php:<version>-apache` image, pinned by digest for each release the extension
ships. It adds a layer of production dependencies, a layer of the application's
files, and an Apache configuration that serves `public/`.

A project may name its own base, pinned by digest. A base with no digest fails.

## Consequences

- The default image holds only the extensions of the official image: it has no
  `pdo_mysql` and no `pdo_pgsql`. A project that needs one builds its own base.
- The image runs as `www-data` on a non-privileged port.
- `composer install` runs with `--no-scripts`, because the assembly runs nothing
  inside the image. A Symfony application warms its cache at first boot.
- `publish` shares the publisher of the other stacks, with its registry and
  release-set rules.

## Alternatives rejected

- **A generated Dockerfile run with `docker build`.** It could install PHP
  extensions, but it requires a Docker daemon, which adds a prerequisite.
- **FrankenPHP.** One binary for the server and PHP, but the base image comes
  from a third party when an official image does the work.
- **`php-fpm` with nginx.** Two processes or two containers to assemble.
