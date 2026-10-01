# ADR 0003 — Publication ops behind a negotiated capability

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/registry` (`protocols/registry`),
  `credential-provider/v1`, capability `publication-v1`

## Context

A publish job runs repository code. When it holds a `publish` credential, so
does every process that code starts, and the provider learns what the run
published only from the release request at the end. The provider cannot tell
which artifacts the run meant to publish, from which commit, or whether each
channel it moves goes forward.

The engine already holds a session with the provider
([ADR 0002](0002-one-credential-call-per-purpose.md)). That session can carry
the publication itself: the engine declares the plan before any upload, uploads
in its own process, and releases the set over the same session.

`credential-provider/v1` is closed and its lines are small: 64 KiB and 5 levels
of nesting. A plan, a release request and its evidence are larger and deeper.

## Decision

The engine publishes through three more ops on the credential provider session,
defined by a capability both sides negotiate at `initialize`.

### Names

The capability is `publication-v1` (`CapabilityPublicationV1`). Its ops are
`resolve`, `open` and `release`. They use the `credential-provider/v1` envelope
and protocol version `1`.

The capability holds when the engine offers it and the provider echoes it
(`NegotiatedCapabilities`). The echo alone decides; no flag or environment
variable does. In a session without the capability, both sides refuse the
three ops as unknown, and every line is a `credential-provider/v1` line under
its bounds. Without the echo, the engine publishes as it does without the
capability.

### Ops

| Op | Payload | Answer payload |
|----|---------|----------------|
| `resolve` | `request`: a `distribution/release-set/v2` resolve request in its channels form | `response`: the resolve response, one head per channel |
| `open` | `plan`: the plan tuple; `ancestry`: one entry per plan channel | `planDigest`: the plan's digest |
| `release` | `planDigest`; `request`: a `distribution/release-set/v2` release request, unchanged; `ancestry`: one entry per request channel; `evidence` | `response`: the release response |

`resolve` and `release` reuse the distribution documents and their strict
decoders and validators (`go.putnami.dev/protocol/distribution`). The engine
binds each answer to its request with `ValidateResolveExchange` and
`ValidateReleaseExchange`.

`credential` with purpose `publish` is unchanged on the wire. The provider
scopes the bearer it answers to the opened plan. The engine applies the host
rule (`Credential.Serves`) to every upload target.

### Plan tuple and digest

The plan tuple (`PublicationPlan`) names every selected member of one
publication before its artifact exists: the namespace, the source revision (a
full lowercase commit), the channels the publication advances, the immutable
channel when it creates one, and one entry per member with its ecosystem,
coordinate, version, source revision and selection fingerprint. Members are
unique and in (ecosystem, coordinate) order.

`planDigest` is `sha256:` followed by the lowercase hex SHA-256 of the plan's
compact JSON without `planDigest`, as Go's `encoding/json` encodes it.
`PlanDigest` computes it. The encoding is the one the engine's release-plan
contract hashes, so a provider that already verifies that contract computes the
same digest. A test pins one vector.

### Order

1. The engine sends `resolve` when the run reads channel heads.
2. It sends `open` after every barrier task has succeeded and before the first
   upload. A barrier task is a task the publication waits for, such as a test
   or a build.
3. It asks for `credential` with purpose `publish` only after `open`. Before
   `open`, a provider refuses with `plan_not_open`.
4. It uploads every artifact in its own process.
5. It sends `release` after the last upload.
6. It ends the session with `shutdown`.

### Idempotency and retry

`open` is idempotent on `planDigest`: the same plan gets the same answer. A
different plan in the same session gets `plan_already_open`.

A provider releases at most once per `planDigest`, which is the idempotency
key. It stores the outcome with the set ref the request derives
(`distribution.DeriveReleaseSetRef`). A `release` with the same `planDigest`
and the same set ref gets the stored outcome. One with another set ref gets
`plan_mismatch`.

The engine sends `release` again once, after a transport error or a timeout,
and in no other case. An answer, a refusal included, is final. When the second
attempt also fails, the engine fails closed and names the `planDigest`, so an
operator can ask the provider for the stored outcome.

`already-current` is a success outcome. A compare-and-swap miss is the
outcome `conflict`, not a refusal.

### Forward only

A channel moves only forward: to a source revision equal to its head's or
descending from it. Three channels are exempt: a channel without a head, the
immutable channel, and a baseline channel, which the publication reads and
does not advance.

The engine reads the ancestry from the repository before any repository code
runs and sends it as a statement (`PublicationAncestry`): the source revision,
the number of commits it read, and for each advanced channel its head's source
revision and whether that head is the source revision or one of its ancestors.
The engine asserts the statement; the provider records it with the release and
does not verify it against the repository. The provider refuses with
`not_forward` at `open`, and again at `release` for the heads its
compare-and-swap matches.

### Bounds

| Line | Bytes | Nesting |
|------|-------|---------|
| `initialize`, `credential`, `shutdown`, in every session | 64 KiB (`MaxCredentialLineBytes`) | 5 levels |
| `resolve`, `open`, `release` | 8 MiB (`MaxPublicationLineBytes`) | 12 levels |

The engine reads each answer under the bound of the op it answers
(`CredentialOp.MaxLineBytes`). Inside a line, a plan encodes to at most 1 MiB,
each evidence list to at most 4 MiB, each distribution document to at most its
protocol's 4 MiB, and an ancestry statement reads at most 1,000,000 commits.
Evidence travels inline in `release`.

### Strictness

The `credential-provider/v1` rules hold: no unknown, duplicate or case-variant
member, no trailing data, no `null`. Inside `request` and `response`, the
distribution protocol decides where `null` is admitted. Every member of a
publication payload is present unless the schema marks it optional, and an
optional member is absent rather than empty, so a decoded payload re-encodes to
the same bytes.

### Evidence

`release` carries `evidence`: each published workload image with its project,
and each published member with its project, identity, digest and the extension
command and step that published it. The provider treats each entry as an
assertion and verifies the artifact itself; it refuses with `artifact_missing`
or `artifact_digest_mismatch`.

### Refusals

`plan_not_open`, `plan_already_open`, `plan_mismatch`, `artifact_missing`,
`artifact_digest_mismatch`, `not_forward`, `conflict`, `channel_immutable` and
`namespace_forbidden` (`PublicationRefusalCodes`). Each matches the
`credential-provider/v1` refusal grammar.

## Rejected alternatives

- **The ops on `credential-provider/v1` without a capability.** v1 is closed,
  and a provider that does not know the ops decodes strictly: it refuses the
  first new line as out of protocol.
- **A flag that selects the ops.** A flag can disagree with the provider. The
  echo states what the provider serves.
- **Raise the v1 bounds for every op.** A credential line has no reason to
  reach 8 MiB, and a larger bound on it widens what a hostile provider can
  send in every session.
- **A separate evidence op.** It adds an ordering rule and a second write per
  release; inline evidence makes `release` one atomic request under one key.
- **The provider verifies ancestry.** It holds no copy of the repository. The
  engine reads ancestry before repository code runs, which is the earliest
  point a run can state it.

## Consequences

- No repository process holds a `publish` bearer under `publication-v1`. The
  engine holds it and uploads.
- Release, template and agent-content archives are not plan members. The job
  that uploads them belongs to the provider extension and keeps its own
  credential; these ops do not cover it.
- A provider stores one outcome per `planDigest`. A run that lost its answer
  learns the outcome by asking again with the same plan.
- A second implementation is checked against the schema and fixture corpus in
  this module, which cover every publication line.
