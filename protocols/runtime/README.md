# Runtime Protocol

## Why

The runtime protocol exists so every job emits one machine-readable stream for logs, progress, diagnostics, metrics, phases, artifacts, metadata, and final results.

That shared event shape is what makes Putnami observable by construction for both humans and automation.

## What

This package defines the versioned runtime JSONL event model, including:

- the top-level event envelope,
- event type discriminators,
- log levels and diagnostic severities,
- phase and result status enums,
- source locations and structured error payloads, and
- decoder / parser / emitter helpers for JSONL streams.

## How

The package supports both producers and consumers of runtime events:

- emitters write one JSON object per line on stdout,
- `ParseEvent()` decodes a single event,
- `Decoder` and `DecodeAll()` consume event streams safely line by line,
- validation helpers and conformance tests keep the stream contract stable.

Use this package when implementing a job runner, reading extension output, or building tooling that reacts to structured runtime events.

## Versions

- **v1** (`v: 1`) — the default when there is no CLI advertisement.
  [`doc/02-event-format.md`](doc/02-event-format.md),
  [`schemas/event.json`](schemas/event.json), corpus `fixtures/{valid,invalid}`.
- **v2** (`v: 2`) — v1 plus the typed `ready` event, which replaces the
  watcher's `"listening http(s)://"` log-substring probe; required for job
  streams handled by CLI contract 3.
  [`doc/05-event-v2.md`](doc/05-event-v2.md),
  [`schemas/event-v2.json`](schemas/event-v2.json), corpus `fixtures/v2/`.

The protocol package defines both versions, but a job stream handled by CLI
contract 3 must use v2; the CLI advertises v2 in
`PUTNAMI_RUNTIME_EVENTS` and accepts only events at that version. A stream may
not mix versions, so its version is **negotiated once**, before its first line:
the emitter stamps the resolved version on everything it writes
([`negotiation.go`](negotiation.go)). No advertisement means v1.

The **outer** session-stream record that wraps these events is owned by
`protocols/cli` (`SessionStreamRecord`, `protocolVersion: 2`) and by nothing
here. The unversioned envelopes this package used to declare beside it
(`job:start` / `job:event` / `job:end` / `session:end`, `stream.go` +
`schemas/stream.json`) were deleted once their last producer was removed:
a producer-less, consumer-less protocol is deleted rather than frozen. See
[`doc/adr/0002-delete-the-producerless-stream-envelopes.md`](doc/adr/0002-delete-the-producerless-stream-envelopes.md).

## Producers and consumers

| Role | Who |
| --- | --- |
| Producers | every job subprocess: the language extensions (`go/extension`, `typescript` and `python` extensions) through `tooling/extension-sdk/jsonl`, the Go and TypeScript framework runtimes, and any third-party extension emitting JSONL on stdout |
| Consumers | the Putnami CLI (session stream, terminal renderer, watch adapter, cached-result replay) and any tool reading a job's stdout |
| Owner of this contract | this project. The CLI owns the OUTER session record; a producer or consumer may not widen the inner event locally |

## Schemas and fixtures

- v1 event envelope: [`schemas/event.json`](schemas/event.json); corpus
  [`fixtures/valid`](fixtures/valid) (3 streams) and
  [`fixtures/invalid`](fixtures/invalid) (3 streams).
- v2 event envelope: [`schemas/event-v2.json`](schemas/event-v2.json); corpus
  [`fixtures/v2/valid`](fixtures/v2/valid) (4 streams) and
  [`fixtures/v2/invalid`](fixtures/v2/invalid) (10 streams).
- Supporting descriptions: [`schemas/payloads.json`](schemas/payloads.json)
  (typed per-verb payloads), [`schemas/workload.json`](schemas/workload.json),
  [`schemas/lifecycle.json`](schemas/lifecycle.json),
  [`schemas/output.json`](schemas/output.json).
- Release-set publication corpus: [`fixtures/release-set`](fixtures/release-set).
  It pins the one successful outcome, dry-run and failure omission, missing and
  duplicate rejection, CAS conflict, and the immutable ref retained across a
  later channel move.
- Prose: [`doc/`](doc), from workload types through the v2 event format.

## Test cases

A test job reports its cases in result data: `ResultData.Data["testCases"]`
(`TestCasesResultDataKey`) holds one entry per case, and
`ResultData.Data["testCasesDropped"]` (`TestCasesDroppedResultDataKey`) counts
the cases it left out, only when it left some out. The entry shape and its
bounds are `TestCase` in [`go.putnami.dev/protocol/cli`](../cli/doc/02-result-v2.md#test-case-records);
[`schemas/payloads.json`](schemas/payloads.json) refers to that definition and
restates none of it. The CLI writes each entry as one `test:case` session-stream
record, so this protocol gains no event type and no version.

## Distribution release-set outcome

A successful managed publication carries exactly one
`distribution.ReleaseSetPublishOutcome` at `ResultData.Data["releaseSet"]`.
On the JSONL wire that is `event.data.data.releaseSet`: the outer `event.data`
is `ResultData`, and its inner `data` is the open per-verb result map. The
release-set fields are not declared in this module. Both event schemas refer to
the canonical
[publish-outcome schema](../distribution/schemas/publish-outcome.json),
and the Go extractor keeps the payload as opaque `json.RawMessage`. A consumer
that needs its fields must parse those bytes with
`go.putnami.dev/protocol/distribution`; runtime intentionally has no production
dependency on that higher-level protocol.

The v2 outcome carries the stored set's immutable `{id, digest}` **and the head
each advanced channel now points at**, under `current`, keyed by channel name,
each with the monotone generation the provider stamped. One release advances
several channels, so a consumer reads that map instead of a single channel
name, and a registry applies a projection only when the generation increases.

The outcome exists only after immutable storage and a successful or idempotent
release (`released` or `already-current`) on **every** listed channel. Dry runs,
partial publication, provider failure, and a CAS `conflict` on any one channel
emit no successful release-set outcome — a conflict writes nothing on any of
them. `RequireReleaseSetPublishOutcome` enforces only runtime's half: one opaque
candidate at the successful result location and cardinality. The consumer must
then validate it with Distribution before use. `RejectReleaseSetPublishOutcome`
enforces absence on dry-run and failure paths. A channel is mutable metadata
and never replaces the outcome's immutable `{id,digest}` in a downstream
handoff.

Each publisher reports what it published with one `published-member` artifact
event, carrying the ecosystem, the coordinate, the version, the verified
artifact digest, and any per-platform digests. The coordinator reconciles those
events against the plan to build the snapshot it releases, so the members —
including the `oci` images — are the proof, and a same-session deploy takes its
workload contract from the released set rather than from a private side channel.

## Compatibility

Within a version, the envelope is **additive**: `additionalProperties` is true,
consumers ignore members they do not recognize, and a producer may attach extra
fields to an event or an artifact payload. Adding a member, an artifact kind, or
a metric name is therefore not a version change.

A **new version** is required to change the vocabulary a consumer must know up
front — adding, removing or repurposing an event `type`, a log level, a phase or
result status, or a diagnostic severity — because a reader that switches on a
discriminator cannot distinguish a value from a newer vocabulary from a
malformed one. That is the whole reason v2 exists for a single new event type.

A version bump means: a new `const ProtocolVersionN`, a schema pinning `v` to
that constant, a corpus under `fixtures/vN/`, drift tests pinning every
vocabulary against the schema, and an entry in the negotiation clamp. The
existing version's documents, fixtures and consumers stay valid — v1 is still
parsed and validated here.

Cross-implementation parity is real and tested: the TypeScript emitter and
parser (`typescript/framework/runtime/src/jobs/events.ts`) validate against
**this package's fixture files**
(`typescript/framework/runtime/test/jobs/events.test.ts` reads
`protocols/runtime/fixtures`), so a shape that drifts in one language fails in
the other.

## Support status

- **Subject**: `go.putnami.dev/protocol/runtime`, kind `protocol`.
- **Status**: `stable`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/runtime`). It owns the envelope, the
  vocabularies, the schemas and the corpus; the CLI, the extensions and the
  framework runtimes are producers and consumers of that authority.
- **Evidence**: both versions are pinned to their schemas by named tests —
  `TestConformance_ProtocolVersion` and `TestConformance_ProtocolVersion2` /
  `TestDriftV2_ProtocolVersionConst`. Every closed vocabulary is drift-tested
  against the schema rather than asserted in prose (`TestDrift_EventTypes`,
  `TestDrift_LogLevels`, `TestDrift_ResultStatuses`,
  `TestDrift_DiagnosticSeverities`, `TestDrift_PhaseActions`,
  `TestDriftV2_EventTypes`, `TestDriftV2_ReadyVocabularies`), and
  `TestDriftV2_V1SchemaFrozen` pins that v1's schema still fixes `v` to 1 and
  still excludes the v2-only `ready` type — so v2 cannot widen v1. The
  corpus is 6 v1 streams and 14 v2 streams, with
  `TestConformance_V2InvalidFixtureCoverage` asserting each v2 reject branch has
  a fixture, and it is validated by a second implementation
  (`@putnami/runtime`) against the same files.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. This
contract is the byte format two processes exchange; a user never chooses it,
sees it, or asks for it. Minting a product feature per technical wire contract
would create a promise with no user on the other end and a second authority
beside the schemas.

The user-facing surfaces that own the outcome are the CLI commands whose output
these events become — `putnami build`, `putnami test`, `putnami serve` and the
machine projections `--output=json` / `--output=jsonl`. A spec belongs with the
change those surfaces are made for; this module records the wire decisions.

Durable decisions recorded here:

- [`doc/adr/0001-one-stream-one-negotiated-version.md`](doc/adr/0001-one-stream-one-negotiated-version.md)
  — a stream speaks one version, resolved once from `PUTNAMI_RUNTIME_EVENTS`
  before its first line, with a total resolution that fails closed to v1.
- [`doc/adr/0002-delete-the-producerless-stream-envelopes.md`](doc/adr/0002-delete-the-producerless-stream-envelopes.md)
  — this package owns the inner subprocess event only; the unversioned outer
  envelopes were deleted rather than frozen once they had no producer.
