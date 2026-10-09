# JSONL Event Format (v2)

Version 2 of the runtime event protocol is version 1's envelope plus **one new
event type**: `ready`, the typed readiness signal. It is additive at the level
of this package — every v1 fixture and every v1 consumer is untouched, and both
versions are parsed and validated here.

What an INVOKER accepts is a separate question, and Putnami's CLI has since
narrowed it: a job stream handled by a CLI that requires the current extension
contract must be v2, and a v1 line from such a stream is rejected. See
[Choosing a version: negotiation](#choosing-a-version-negotiation) — the table
there describes what an emitter STAMPS, which is negotiated per stream.

- Schema: [`schemas/event-v2.json`](../schemas/event-v2.json)
  (`$id` `https://putnami.dev/schemas/protocol/runtime/event-v2.json`)
- Go types: [`event_v2.go`](../event_v2.go), rules in
  [`validate_v2.go`](../validate_v2.go)
- Corpus: [`fixtures/v2/{valid,invalid}`](../fixtures/v2)
- v1 remains documented in [`02-event-format.md`](02-event-format.md)

## Who owns what

A `--output=jsonl` line used to blur two layers. They are now split, and the
split is recorded in
[`adr/0002-delete-the-producerless-stream-envelopes.md`](adr/0002-delete-the-producerless-stream-envelopes.md):

| Layer | Owner | Version |
|-------|-------|---------|
| The **outer** session-stream record (`task:start` / `task:event` / `task:end` / `session:end`) | `protocols/cli` — `SessionStreamRecord` | `protocolVersion: 2` |
| The **inner** subprocess event a `task:event` record carries in its `event` member | `protocols/runtime` — this document | `v: 2` |

This package used to declare the outer envelopes too (`job:start`, `job:event`,
`job:end`, `session:end`, in `stream.go`), unversioned and frozen beside the
CLI's versioned successor. Removing the CLI's v1 JSONL renderer left them with
no producer and no consumer, so they were deleted — with their schema and their
corpus — rather than kept as a compatibility promise nothing could honor. The
outer record now has exactly one owner: see
[`protocols/cli/doc/02-result-v2.md`](../../cli/doc/02-result-v2.md).

## Why a version for one event type

The vocabulary is what a consumer must know up front. A reader that switches on
`type` cannot tell *"this runtime never reports readiness"* from *"this runtime
reports it and I do not understand the type"* unless the envelope says which
vocabulary it speaks. So:

- `v: 1` admits exactly the nine v1 types. A `ready` event stamped `v: 1` is
  rejected as an unknown type — that is what keeps v2 additive instead of
  retroactively widening v1.
- `v: 2` admits those nine plus `ready`. Nothing else changes: every v1 event
  shape is byte-identical at v2.
- A stream **may not mix versions**. One subprocess emits one contract, so a
  stream that changes version mid-flight is rejected (`mixed-protocol-version`)
  rather than interpreted per line.

## The `ready` event

```json
{
  "v": 2,
  "type": "ready",
  "time": "2026-07-28T09:00:01.000Z",
  "data": {
    "target": "server",
    "name": "api",
    "endpoints": [{ "scheme": "http", "host": "localhost", "port": 3000 }],
    "durationMs": 420
  }
}
```

| Field | Required | Meaning |
|-------|----------|---------|
| `data.target` | yes | **What** became ready. `server` = a network listener; `workload` = the whole process finished startup. Closed vocabulary. |
| `data.name` | no | Disambiguates several targets of one workload (`api`, `admin`). |
| `data.endpoints` | for `server` | Addresses the target accepts traffic on, canonically ordered and unique. |
| `data.durationMs` | no | Milliseconds from process start to readiness. |

An endpoint is `{ scheme, host, port, path? }` with `scheme` drawn from `grpc`,
`http`, `https`, `tcp`, a port in `[1,65535]`, and a `path` that starts with
`/` when present. **The URL is derived** (`ReadyEndpoint.URL()`), never carried,
so an endpoint cannot ship a `url` that disagrees with its own members. A
carried view of structured members is a second authority for one fact, and the
two are equal only by accident.

`target` distinguishes component readiness from process readiness because they
are not interchangeable: a `server` claim without an address is not actionable
(hence the endpoint requirement), while a worker that binds nothing is still
legitimately ready.

### Why it replaced the substring probe

Readiness used to be decided by matching `"listening http://"` in a job's log
text, in one compatibility adapter
([`tooling/cli/internal/watch/serve_ready.go`](../../../tooling/cli/internal/watch/serve_ready.go))
the file itself documented as a temporary seam. That probe was wrong in
three ways this event fixes: it read a human message as a machine signal (any
wording change silently broke the watcher), it could not say *what* became
ready, and it discarded the address it had just matched instead of handing it to
the renderer.

The consumer contract for the watcher is: **arm on the first `ready` event of
the serve iteration**, which is exactly the semantics of the first-matching-line
probe it replaced. First-party runtimes emit the event (see "How a workload
announces readiness" below), and the watch adapter consumes it; the probe is
gone. Emission and consumption landed as separate changes on purpose, so that
reverting the emission cannot resurrect the log scraping.

A `ready` a consumer cannot act on is not a readiness claim, so the watcher
requires the v2 envelope the type is admitted by and a payload whose `target` is
in the closed vocabulary. Every rejection fails **closed**: an unarmed watcher
costs a serve iteration its hot reload, while arming early restarts a server
that has not bound its port.

### Determinism

Endpoints are emitted and validated in one canonical total order — scheme, then
host, then port, then path, compared by **byte order, never locale order** — and
duplicates are rejected. A workload that binds its listeners in a
race-dependent order therefore still produces one deterministic line, which is
what makes a digest over the event stable.
`Emitter.Ready` sorts a copy of the caller's slice, so canonical order is
guaranteed by construction rather than by convention.

## Diagnostics

| Code | Meaning |
|------|---------|
| `invalid-version` | `v` is neither 1 nor 2 |
| `invalid-event-type` | the type is not in the vocabulary **this version** admits |
| `mixed-protocol-version` | one stream carries more than one `v` |
| `required-field` | `data`, `data.target`, a `server`'s endpoints, or an endpoint's `scheme`/`host` is absent |
| `invalid-enum` | `data.target` or an endpoint `scheme` outside its closed vocabulary |
| `invalid-value` | a port out of range, a relative endpoint path, a negative `durationMs` |
| `invalid-ready-data` | `data` is not a readiness payload |
| `non-canonical-order` | endpoints are not in canonical order |
| `duplicate-endpoint` | one address is claimed twice |

## Choosing a version: negotiation

A stream may not mix versions, so its version is decided **once, before the
first line** — never per event. The invoking CLI advertises the highest version
it accepts in a reserved environment variable, and the emitter stamps the
resolved version on everything it writes:

```
PUTNAMI_RUNTIME_EVENTS=2
```

| Advertised | Emitter stamps | Why |
|---|---|---|
| absent / blank / unparsable | `v: 1` | a 0.2.x CLI, or a hand-run extension binary, must receive the byte-identical v1 stream it can parse |
| `0`, `-1`, `1` | `v: 1` | below the floor is the floor |
| `2` | `v: 2` | readiness may travel |
| `3`, `99` | `v: 2` | a consumer that accepts a newer version accepts every version below it, so clamping down is safe |

Decimal values outside the supported integer range are treated as unparsable and therefore select `v: 1`.

**What an invoker accepts below its advertisement is the invoker's policy, not
this table's.** The table says what an EMITTER stamps. Putnami's CLI accepts
exactly the version it advertises, and rejects a v1 line from an extension job
stream. That is safe only because the manifest loader rejects any extension
below the required contract first, so a v1 line means the stream contradicts the
manifest rather than that the emitter is old. Two skews closed with it, both of
which used to be silent: an old-SDK extension forwarding the `putnami.ready` log
marker as ordinary context noise instead of a typed `ready` event, and a
v1-stream serve job whose watcher never armed.

One helper resolves this on both sides — `NegotiatedVersion` in
[`negotiation.go`](../negotiation.go) — so the CLI writer
(`tooling/cli/internal/jobs/runner.go`) and the SDK reader
(`tooling/extension-sdk/jsonl`) cannot disagree. The TypeScript mirror is
`negotiatedRuntimeEventVersion` in
[`typescript/framework/runtime/src/jobs/events.ts`](../../../typescript/framework/runtime/src/jobs/events.ts).

It travels in the environment rather than the job context file because the
advertisement must survive a process boundary the context file does not cross:
`--putnamiContext` reaches the extension binary alone, while a serve job's
environment is inherited by the workload the extension spawns.

`Emitter.Ready` on a v1 stream is a hard error (`ErrReadyUnsupportedVersion`),
never a downgrade: a `ready` line stamped `v: 1` is an unknown type, and a
`v: 2` line inside a v1 stream is `mixed-protocol-version`. Both are wire
violations, so the only correct behavior is to refuse.

## How a workload announces readiness

A served workload's stdout is a **log** stream, not an event stream — the
extension that spawned it re-emits each line as a `log` event — so the workload
cannot write runtime events itself. It announces readiness by attaching a
machine-readable payload to the log record it already writes when it starts
listening, under one reserved key:

```json
{
  "severity": "INFO",
  "message": "⚡️ listening http://localhost:3000",
  "durationMs": 12,
  "putnami.ready": {
    "target": "server",
    "endpoints": [{ "scheme": "http", "host": "localhost", "port": 3000 }],
    "durationMs": 12
  }
}
```

The key (`ReadyLogKey`) and the payload rules are defined once in
[`ready_marker.go`](../ready_marker.go) and mirrored in TypeScript
(`READY_LOG_KEY`). The extension SDK's shared forwarder
(`tooling/extension-sdk/jsonl/forward.go`) recognizes it and, when its own
stream is negotiated at v2, emits the typed `ready` event **after** the log
event — so the console still shows the human line first — and strips the marker
from the forwarded log's context so the machine channel never
prints. Because the forwarder is shared, all three first-party serve wrappers
gain the capability from one place, including Python's, which has no framework
of its own.

The Go and TypeScript application frameworks also announce that the whole
application finished startup. Their `🤖 ready` log record carries a `workload`
claim, written only after every plugin starter and every module start hook
returned, and never when one of them failed. It lists the endpoints the
application's plugins bound, so it names the same address as the `server` claim
before it:

```json
{
  "severity": "INFO",
  "message": "🤖 ready",
  "durationMs": 41,
  "putnami.ready": {
    "target": "workload",
    "endpoints": [{ "scheme": "http", "host": "localhost", "port": 3000 }],
    "durationMs": 41
  }
}
```

A worker that binds nothing writes the same claim without `endpoints`. A
listener whose address is not addressable writes no `server` claim at all: the
`workload` claim alone announces that the application started.

An HTTP application therefore emits two `ready` events per start: the `server`
claim when its listener binds, then the `workload` claim once startup completed.
A consumer that needs an address arms on the first; the watcher still arms on the
first event of an iteration and ignores the rest. A consumer that needs the
application started waits for the `workload` claim:
`putnami qualify --target local` does, when the workload's route inventory
declares no readiness route, because a listener answers (an auth denial, a
`404`) long before the application finished starting. A consumer that routes
traffic keeps the address it already holds when a claim carries none.

A marker that would produce an invalid `ready` event yields **no** readiness
rather than an invalid line: losing one signal costs a watcher its fast path,
while an invalid line costs the consumer the whole stream.

Restart coverage falls out of that design: the forwarder holds no per-stream
memory of having seen a marker, and every restart loop — the CLI's serve
iteration, `go/extension`'s `runWithWatch`, `python/extension`'s watch loop —
spawns a fresh process whose output flows through the same forwarder. Readiness
is therefore announced once per serve **iteration**, which is why more than one
`ready` in a stream is legal (`fixtures/v2/valid/serve-restart.jsonl`).

## What readiness does NOT require

- **A `ready` event is never mandatory.** A v2 stream with no `ready` line is
  valid (`fixtures/v2/valid/all-v1-types.jsonl`), because a job that binds
  nothing has no readiness to announce. What a serve job LOSES by not announcing
  it is the watcher: the arming signal is the event, and there is no fallback.
- **Nothing about readiness is required of this package's v1 corpus.**
  `fixtures/{valid,invalid}` — the cross-language corpus the TypeScript mirror
  validates against too — is untouched, and the v2 corpus lives in
  `fixtures/v2/`. A `ready` event stamped `v: 1` is rejected as an unknown type;
  that rejection is what makes v2 a version rather than an unannounced widening.
- **Readiness is not an invoker requirement either.** The CLI advertises the
  version it accepts and rejects a stream that answers below it, but it never
  requires the stream to contain a `ready` event.
