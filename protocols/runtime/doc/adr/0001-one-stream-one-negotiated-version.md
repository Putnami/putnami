# ADR 0001: One stream speaks one version, negotiated before its first line

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runtime` (`protocols/runtime`)

## Context

A job subprocess writes JSONL events to stdout for the process that invoked it,
and cannot know which event vocabulary that reader understands. A reader that
switches on `type` cannot tell "this runtime never reports readiness" from "this
runtime reports it and I do not understand the type" unless the envelope names
its vocabulary. So every event carries a version, and the question is where the
version comes from and how often it may change.

## Decision

**A stream speaks exactly one version, chosen once, before its first line.**
Mixing versions within a stream is a validation error
(`mixed-protocol-version`), and the emitter stamps the resolved version on
everything it writes.

The version is resolved from one reserved environment variable:

```
PUTNAMI_RUNTIME_EVENTS=<highest protocol version the invoker accepts>
```

The environment, not the job context file, carries it, because a serve job's
environment reaches the workload the extension spawns, while `--putnamiContext`
reaches the extension binary alone.

`NegotiatedVersion` is total, so an emitter never handles a bad advertisement:

- absent, blank, unparsable, or below v1 → v1;
- above the highest known version → the highest known version.

v1 is the fail-closed direction: an old invoker, a directly executed extension,
and a corrupted value all receive a v1 stream they can parse, and a `ready`
line never reaches a reader that would reject the stream for it.

**An emitter answers at the advertised version, not under it.** The Putnami CLI
accepts exactly what it advertises, because an extension at the current contract
must read this variable and answer at it. A lower-versioned line means the
stream disagrees with the manifest.

## Rejected alternatives

- **Version each line independently.** A consumer would hold every vocabulary
  at once, and the unknown-type ambiguity returns on every line.
- **Advertise through the job context file.** It never reaches the spawned
  workload, whose readiness event the negotiation exists for, and shell
  extensions cannot parse JSON.
- **A capability handshake** (a list of types or a structured document). No
  consumer needs sub-version granularity; it would need its own parser and
  version.
- **Let an emitter undercut the advertisement.** The reader could not tell a
  legitimately old emitter from a broken new one.
- **Infer the version from the events** (a stream with `ready` is v2). An
  identical run with no readiness signal would be a different version.

## Consequences

- An emitter resolves its version before writing anything. A stream is never
  upgraded in flight.
- The variable name and the clamping live in [`negotiation.go`](../../negotiation.go),
  and both sides go through it.
- `ProtocolVersion` (1) names the v1 vocabulary; it is not "the version to
  stamp". Producers call the negotiation.
- A corrupted advertisement degrades to v1 silently, so a misconfigured invoker
  looks like an old one.
