# ADR 0001 — The SDK comes from Microsoft's releases, and an application image copies onto the official .NET runtime image

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/dotnet`, the SDK a task runs, and the `serve`, `run`,
  `package` and `publish` commands

## Context

A .NET repository states its SDK in `global.json`: a version and a
roll-forward policy that accepts a range of SDKs. A machine with no SDK, or
with none that policy accepts, cannot lint, test or build. The stack core
lets an extension pin a toolchain in the workspace lock by SHA-256 (ADR 0003
of the stack core), or install one when a task needs it (ADR 0004).

Microsoft publishes every SDK as an archive per platform, and a release
metadata document per channel that names each archive's URL and SHA-512. It
publishes no SHA-256, and a `global.json` may state any SDK of any channel.

An application needs the .NET runtime of its target framework. `@putnami/go`,
`@putnami/typescript` and `@putnami/php` assemble their images with the OCI
assembler of the extension SDK, with no Docker daemon, onto a base pinned by
digest.

## Decision

- **The SDK is installed at task time** (`Stack.Provide`), when no SDK on
  `PATH` satisfies `global.json`. Among the SDKs an earlier task installed
  for the same platform, the one the policy picks is reused with no network.
  Otherwise the extension reads the release metadata and picks the SDK the
  .NET host would: the stated one for `disable`, `patch` or no policy, the latest patch of the stated feature band, else of
  the next one, for `feature`, `minor` and `major`, the highest for the
  `latest*` policies, and with no `global.json` the latest SDK of the channel
  the extension names. It installs, beside it, the latest ASP.NET Core
  runtime, which holds the .NET runtime, of every channel from 8.0 up to the
  SDK's, whatever its support phase, since an SDK carries only its own
  runtime and a project may target an older one. Each
  archive is checked against the SHA-512 the metadata states and extracted
  under `<PutnamiHome>/tools/dotnet/sdk-<version>_<platform>`, where
  `<platform>` is the runtime identifier, `linux-musl-*` on a system whose
  `/bin/sh` runs on musl. An offline run installs nothing.
- **`serve` and `run`** build the project under the stack-root lock, then run
  `dotnet run --no-build` on the project file, with no launch profile, `serve`
  on the port flag through `ASPNETCORE_URLS` and `PORT`.
- **`package`** runs `dotnet publish` for the target platform,
  framework-dependent, and assembles an image of the published files under
  `/app` onto the official `mcr.microsoft.com/dotnet/aspnet` image of the
  target framework when the published `runtimeconfig.json` needs ASP.NET
  Core, `runtime` otherwise, pinned by digest for each runtime the extension
  ships. The publish runs under the stack-root lock, since it restores and
  builds the referenced projects in place. It runs as the image's
  non-root `app` user on port 8080. A project may name its own base, pinned
  by digest; a base with no digest fails.
- **`publish`** shares the publisher of the other stacks, with its registry
  and release-set rules.

## Consequences

- The SDK's integrity rests on the release metadata Microsoft serves over
  HTTPS, as `dotnet-install` does; the archive's bytes are checked against it.
- The workspace lock pins no SDK, so two machines may install two SDKs the
  same `global.json` accepts; the cache key carries the stated version, as
  ADR 0010 of the stack core settles.
- An image holds no SDK and no source: only the published output.

## Alternatives rejected

- **The SDK in the workspace lock.** Every release the extension supports
  would need its SHA-256, which Microsoft does not publish, and a
  `global.json` naming another SDK would find no pin.
- **The `dotnet-install` script.** A script fetched and run at task time,
  where a checked archive suffices.
- **A self-contained or trimmed publish.** Larger images, or code that
  reflection-based libraries break.
- **A generated Dockerfile with `docker build`.** It needs a Docker daemon.
