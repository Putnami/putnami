# Runner contract

This package owns the credential-free wire contracts of optional runner
execution: the source manifest, the execution request (snapshot-addressed in
version 1, commit-addressed in version 2), the provider RPC and the session
bundle. `runner-provider` is the reserved extension command name.
Nothing here grants execution authority or source access; the executing
Putnami engine remains the only planner, scheduler and verdict producer.

## Source manifest (v1)

A `SourceManifest` has `version: 1` and an `entries` array, including `[]` for an
empty snapshot. Each entry describes either a regular file (`path`, `kind`,
`digest`, `size`, `mode`) or a symlink (`path`, `kind`, `target`), optionally
followed by `bound: true`. Directories are implicit. Omitted files are absent,
so deletion changes the manifest. A regular file's digest is `sha256:` followed
by the lowercase hash of its exact bytes; its size is required even when zero.
Modes are `0644` or `0755`, preserving the executable distinction independently
of the producer's umask. Symlink targets retain their exact relative spelling:
resolving `a/..` after following `a` can differ from removing these components
lexically. A `bound` entry is a path Git ignores in the source worktree that was
captured because a planned task declares it as an input; the member is present
only as true (an explicit `false` is rejected as non-canonical) and is part of
the canonical bytes, so binding a path changes the source digest.

### Content identity and canonical bytes

`SourceDigest` is SHA-256 of `CanonicalSourceManifest`, with a `sha256:` prefix.
The canonical bytes are compact UTF-8 JSON without a trailing newline. Object
members appear in the order shown above; the root order is `version`, `entries`.
Entries are already sorted by path's UTF-8 bytes, strictly increasing. Producers
must sort before validation; receivers reject unsorted manifests. String escaping
matches Go `encoding/json`: quotes and backslashes are escaped, `<`, `>` and `&`
use lowercase `\u` escapes, as do U+2028 and U+2029; other Unicode remains UTF-8.
Numbers use decimal integer notation. These rules are part of v1 and cannot be
changed without changing the contract.

The separate `GitContext` contains only `head`, `branch` and `dirty`; source
identity cannot depend on this type. Empty head means unborn Git history, and
empty branch means detached HEAD. Full SHA-1 and SHA-256 object IDs are accepted.
Branch names follow a bounded Git ref spelling. No URL, `.git` directory, Git
configuration, hooks, environment or credentials belong to this context.
Identical source under different commits therefore has identical source identity.
Execution identity and task cache keys must separately retain any relevant Git,
engine, platform, selection and policy inputs; source equality does not prove
that a previous execution applies to another invocation.

### Admission rules

Decoders reject unknown or case-aliased fields, duplicate JSON members, explicit
null, invalid UTF-8, replacement characters, malformed Unicode escapes, extra
JSON values and nesting deeper than eight levels. File and symlink shapes are
disjoint; a zero-valued extra field is still rejected. Schemas describe the
structural shape; these semantic checks are required in addition to JSON Schema.

Source paths are canonical relative slash paths: no empty, dot or parent
components, absolute paths, backslashes, control characters, Windows-reserved
characters or names, trailing dot/space, or `.git` component in any letter case.
Paths and implicit directories must not have aliases differing only by lowercase
mapping. File/symlink ancestors are forbidden. Symlink targets may contain dot
and parent components but must remain relative and pass the component safety
rules. Resolution expands links component by component, rejects case aliases,
regular-file traversal, cycles, excess depth and workspace escapes. Dangling
links to a safe internal path are allowed. Filesystem materializers must still
use rooted operations and reject filesystem-specific alias/collision behavior;
validation does not make an arbitrary destination safe against concurrent edits.

Limits: 32 MiB JSON; 100,000 entries; 1 GiB per regular file; 16 GiB aggregate
regular-file bytes; 1,024 UTF-8 bytes per path/target; 255 bytes per component;
64 components per path/target, resolved depth and symlink expansions.

## Execution request (v1)

`ExecutionRequest` is the one authority a provider executes. Raw argv never
travels beside it. It has `version: 1` and seven blocks, in this order:

| Block | Content |
| --- | --- |
| `protocol` | `version` (1) and the sorted `capabilities` negotiated for the submission. |
| `source` | `digest` (manifest digest), `indexDigest` (sha256 of the captured Git index listing), `git` (the context above), optional `tree` (the existing HEAD-bound worktree fingerprint: `fingerprint`, `dirty`, `headSHA`), `versions` (resolved version lines sorted by unique `line`: `base`, `full`, `sha`, `branch`, `suffix`, `tag`, `tagged`, `dirty`), and optional `bound` (the sorted unique source paths the submitting engine admitted as required ignored inputs, each a `bound` manifest entry; absent when none, never an empty list). |
| `invocation` | `commands` (1–16 unique identifiers), `params` (typed, see below), `flags` (the effective execution flags, every member present, placement excluded), `cwd` (workspace-relative, `.` for the root), optional `providers` (the sorted unique invocation providers, `install` and `publish`, for which the executing engine may ask the workspace's credential provider; absent when none, never an empty list), optional `publication` (`barrier`: the sorted unique invocation commands every publication task waits for; see below). |
| `selection` | `requestedMode` and `mode` (`all`, `impacted`, `projects`), `scoped` (equal to `mode != all`), sorted unique `projects` (non-empty), `baseline` (a full commit, required and only allowed for `impacted`), `baselineSource`, sorted `changedPaths`, bounded `diagnostics`, sorted `noCacheProjects`, optional `taskScopes` (selected project id → the sorted tasks of it the change reaches, each `[<extension-project-id>#]<command>~<step>`, or the bare extension project ids of its tool scope when the submitter resolved no task declarations; the executing engine plans those tasks and their predecessors on that project and nothing else; a project absent from the map runs every task). |
| `plan` | `tasks` sorted by identity key: `identity` (the `protocols/cli` task identity, `key` equal to `project.id + ":" + task.name`), sorted `dependsOn` and `serializeAfter` naming other tasks of the plan, `contractDigest` (`<format>:<64 hex>` or empty), `deadlineMs` (≥ 1), `cacheable`, `resources` (`heavy`, positive finite `cpuWeight`, `reads` and `writes` sorted by scope then id). Cache presence is not part of the plan. At least one task; an empty plan is a local no-op, never a submission. |
| `environment` | `cli` (`source` is `workspace`, `published` or `unpinned`; `version` only for `published`), `extensions` and `toolchains` sorted by unique `name`, `platform` (`os`, `arch`). |
| `control` | `caller` (`cli`), `idempotencyKey` (32 lowercase hex), `deadline` (RFC 3339 UTC instant ending in `Z`). |

`invocation.providers` carries the caller's `--providers` choice, so the
executing engine consults the credential provider
([`protocols/registry`](../registry/README.md), `credential-provider/v1`) for
exactly the purposes the caller enabled: `install` enables the `read`
credential and `publish` the `publish` credential. A provider the request does
not name is never consulted. A client sends the member only to a runner
provider that echoed `invocation-providers-v1` at `initialize`, and offers that
capability only for such a request; the CLI refuses `--where remote` with
`--providers` before it submits when the provider did not echo it.

`invocation.publication` authorizes publication tasks. With it, `barrier`
names at least one listed command, none of them a publication command, and
every task of a barrier command is a transitive `dependsOn` predecessor of
every publication task. Each side checks the direction it can classify:

- `ParseExecutionRequest` classifies by command name. It refuses a task of a
  publication command (`deploy`, `publish`) without the block, and a barrier
  that such a task does not wait for. It accepts the block over a plan with no
  task of a publication command, because a task that writes to a registry
  under another command, such as an image push under `build`, is invisible to
  it (fixture `valid/publication-under-another-command.json`).
- The executing engine classifies by each task's declared traits and effects.
  It refuses a task with registry or cloud effects without the block, a
  barrier that such a task does not wait for, and the block over a plan with
  no such task. The block is then present exactly when the plan publishes.

The CLI submitter never sets it in this release: remote placement still
refuses every task with registry or cloud effects before a request is built.

Both members are omitted when empty, so a request without them keeps the
canonical bytes and the `ExecutionInputDigest` it had before they existed.

A parameter is `{ "type": T, "value": V }` with `T` one of `string`, `bool`,
`int`, `float`, `strings`; `V` must be a JSON string, boolean, integer (no
fraction, no exponent, machine-sized), finite number or array of strings
respectively. The executing engine receives exactly the named Go type
(`string`, `bool`, `int`, `float64`, `[]string`), never a generic decoding, so
the parameter hash it computes equals the submitter's.

Canonical bytes follow the source-manifest rules: compact JSON in the member
order above, Go escaping, maps sorted by key. Decoders reject unknown, missing
or null members at every object level, duplicate members, trailing values and
nesting beyond eight levels (the document budget, `MaxDocumentDepth`).

`ExecutionInputDigest` is `sha256:` over the canonical JSON of
`{"domain":"putnami/runner/execution-input/v1","protocolVersion":1,"source":…,"invocation":…,"selection":…,"environment":…}`
where `selection` carries every selection member except `diagnostics`. It
excludes `plan` (derived, which is why `resources.json` and `projects.json`
pin the same digest), `control` (per submission), negotiated capabilities and
the selection diagnostics (human notices). It establishes applicability; it
is not a task cache key.

## Execution request (v2)

`CommitRequest` is the commit-addressed, engine-planned request. A caller that
holds a commit and no snapshot, such as a hosted CI service, names the commit
and the selection it wants, and the executing engine plans the checkout
itself. It has `version: 2` and five blocks, in this order
(`schemas/execution-request-v2.json`):

| Block | Content |
| --- | --- |
| `protocol` | As in version 1. |
| `source` | `commit` (the full lowercase commit id, 40 or 64 hex digits, that the checkout's HEAD must equal) and optional `base` (the full commit an impacted selection is measured against, of the same length; present exactly when `selection.mode` is `impacted`). |
| `invocation` | The version 1 block, under the same rules, naming no command of `UnportableCommands`. |
| `selection` | `mode` (`all`, `impacted`, `projects`) and optional `projects`: the sorted unique selectors of a `projects` selection in the `--projects` grammar, one per entry, none holding a comma, spelling a mode (`*`, `[impacted]`) or carrying surrounding whitespace. Present, and non-empty, exactly in the `projects` mode. |
| `control` | As in version 1, except that `caller` is `cli` or `ci`. |

Each version pairs an address with a planner. A snapshot has no Git history,
so an engine that receives one can neither measure an impacted selection nor
stamp versions: the submitter does both and freezes the plan. A frozen plan
names no commit it was planned from, so it never travels with a commit
address. Decoders refuse every mix with an error that names it: a version 2
document with `plan`, `environment`, a version 1 source member (`digest`,
`indexDigest`, `git`, `versions`, `tree`, `bound`) or a frozen selection
member; a version 1 document with `source.commit` or `source.base`, or with
caller `ci`. Version 1 stays the CLI's alone.

Canonical bytes follow the version 1 rules. `ParseBoundRequest` reads the
root `version` and hands the document to the parser of that version, so a
version 1 document parses, encodes and digests exactly as
`ParseExecutionRequest` reads it. It refuses any other version.

A version 2 request reaches an engine with no submitting CLI in front of it,
so `ValidateCommitRequest` refuses the commands no portable run carries
(`UnportableCommands`): `serve`, `run` and `compose` stream a live workload,
`qualify` reaches one, and `format` rewrites the source. The submitting CLI
refuses the same commands before it builds a version 1 request; version 1
validation is unchanged.

`CommitInputDigest` is `sha256:` over the canonical JSON of
`{"domain":"putnami/runner/execution-input/v2","protocolVersion":1,"source":…,"invocation":…,"selection":…}`.
It excludes `control` and the negotiated capabilities. Its domain differs
from version 1's, so no version 1 input digest names a version 2 input.

Without a plan, `ValidatePublication` has nothing to check on the request.
The executing engine holds the plan it makes to both version 1 checks when
that plan publishes, and runs a plan that publishes nothing with or without
`invocation.publication`.

A version 2 request travels only through the bound-request channel
(`PUTNAMI_RUNNER_REQUEST`): `submit` carries version 1 only, and no capability
negotiates version 2. The executing engine refuses a checkout whose HEAD is
not `source.commit` or whose tracked files differ from it, and a planned task
whose cwd leaves the checkout. The session it
records states the remote placement and no `placement.provenance`, because
the `protocols/cli` provenance block requires a source digest, which this
request does not have. [ADR 0006](doc/adr/0006-commit-addressed-engine-planned-request.md)
records the decision.

## Provider RPC (v1)

A provider is the extension command named `runner-provider`, spoken to over its
stdin/stdout as one JSON request per line and one JSON response per line:

```json
{"protocolVersion":1,"id":1,"op":"initialize","payload":{…}}
{"protocolVersion":1,"id":1,"ok":true,"payload":{…}}
```

`id` is positive and correlates the response; a failed response carries
`ok: false` and `error: {code, message}`. Envelopes are parsed with their own
nesting budget of twelve levels (`MaxEnvelopeDepth`): the deepest admitted
document — a submit payload whose task declares resources — sits at depth 9
inside its envelope, two levels below the eight-level document budget. Ops: `initialize` (protocol,
`exchangeDir`, capabilities, opaque workspace → `providerName`,
`providerVersion`, echoed `capabilities`, `ready`, `reason`), `prepare` (source
digest and manifest → `missingBlobs`), `submit` (request and manifest →
`attempt`, `state`), `lookup` (`idempotencyKey` → `attempt`, `state`,
`executionInputDigest`, all absent when the key was never accepted), `follow`
(`attempt`, `cursor` → `state`, `cursor`, `records`, optional `exitCode`,
`reason`), `cancel` (`attempt` → `state` observed once the request was
applied), `fetch` (`attempt` → `bundle`), `shutdown`. Bytes never cross the
pipe: source blobs and bundle files sit under the exchange directory at
`<dir>/<hex[0:2]>/<hex>` by digest. States are `queued`, `preparing`,
`running` and the terminal `completed`, `failed`, `canceled`. A client
submits only to a provider that reports protocol version 1, `ready: true`,
and echoes `execution-request-v1` and `session-bundle-v1`, and a request that
carries `invocation.providers` only to a provider that also echoes
`invocation-providers-v1`.

One idempotency key (`control.idempotencyKey`) is one attempt: a repeated
`submit` under an accepted key answers with the attempt it already names and
starts nothing, and a client asks `lookup` before it submits so a lost
acknowledgement never creates a second attempt. An intentional retry is a new
key. `cancel` answers an observation, never a promise: a terminal state the
attempt had already reached, or the non-terminal state it is in while the
provider terminates its process tree; the client follows it to the terminal
state. A cancel racing a completion converges on the outcome the provider
recorded.

Each output record is `{cursor, stream, line}` where `line` is one unchanged
line of the executing engine's stdout or stderr; cursors are strictly
increasing. A `follow` answer carries at most `MaxFollowRecords` (1024)
records; a client that reconnects passes the last cursor it persisted and
drops any record at or below it, so a replay never duplicates a line. Control
envelopes never rewrite canonical `protocols/cli` records.

## Session bundle (v1)

`{attempt, exitCode, sessionId, sessions}`. `sessionId` names the gate session
and must be listed first, or is empty when the executing engine refused before
recording one. Each session is `{id, parentId?, files}` with the CLI's
session-id spelling, a parent that is itself in the bundle, and admitted files
`session.json` (required), `plan.json`, `events.jsonl`, `report.json`,
`spec-verification.json`, `run-report.json`, each `{name, digest, size}`.
Importers verify every digest and size, validate `session.json` and `plan.json`
against the `protocols/cli` contract, and require every document to name its
session. Repeating an import of identical content is safe; a different record
under an existing id is refused.

## Conformance

`fixtures/source-manifest`, `fixtures/execution-request` and
`fixtures/session-bundle` hold valid and invalid corpora; `digests.json` pins
canonical content and execution-input identities. The execution-request
corpus holds both versions, the version 2 documents prefixed `commit-`, and is
read through `ParseBoundRequest`. The Go decoder consumes the
corpus. There is no TypeScript runner runtime in this repository yet; any
future reader must consume the same corpus and match its bytes/digests before
claiming cross-runtime conformance.

Run `./putnamiw test --projects go.putnami.dev/protocol/runner --enforce-coverage`.

## Support

`preview`, as recorded in the [reviewed support catalog](../../putnami.support.json).
The CLI consumes every contract here as the submitting and executing engine;
its internal source store produces, validates and materializes the manifest,
and its provider client and conformance provider speak the RPC. There is no
external runtime consumer or TypeScript binding yet (`parity: unsupported`).
Stable support still requires those consumers and their shared-corpus
qualification, plus an explicit compatibility window. Cloud execution and
trusted cache reuse remain out of scope here.
