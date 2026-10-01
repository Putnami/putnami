# ADR 0001 — A contract artifact is a build output, never a startup side effect

- **Status**: accepted
- **Scope**: `go.putnami.dev/proto` (`go/framework/proto`)

## Context

The proto plugin renders a `.proto` document from the api plugin's routes. It
is a reviewable artifact, committed at `schema/api.proto`. A running service
that writes it would write into whatever directory it started from, and fail
to start on a read-only filesystem.

## Decision

`Configure` renders the document into memory and touches no file. Describe and
normal startup take the same `Configure` path. Build-time `Describe` is the
only automatic writer: it writes `DefaultOutputPath` (`schema/api.proto`) below
the output directory the runner supplies, because the runner owns where build
output lands.

`WriteTo(path)` is the explicit export outside `putnami build`; an empty path
writes to `PluginOptions.Output`, which affects nothing else.

RPC names are part of the published contract. They derive deterministically
from method and path, and collisions are disambiguated deterministically, so
regenerating the document never renumbers a service.

## Rejected alternatives

- **Write at startup and ignore the error.** It still writes to an arbitrary
  directory, and a real export failure becomes silent.
- **Gate the startup write on a flag.** Its safe value is "off" everywhere.
- **Write to a temporary directory.** Nobody reads it.
- **Delete `Output`.** Callers outside `putnami build` still need a
  destination.

## Consequences

- A workload that never runs `putnami build` or calls `WriteTo` gets no
  `.proto` file.
