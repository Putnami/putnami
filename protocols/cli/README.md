# protocol/cli

The canonical **Putnami CLI contract**: the process exit-code taxonomy, output
modes, the structured result envelope, and the error classification that maps
failures to exit codes.

Before this package, each CLI surface — the framework CLI (`tooling/cli`), the
extension SDK (`tooling/extension-sdk`), and cloud's `cli-core` — re-implemented
these conventions independently, so `--output`, `--json`, and exit codes meant
subtly different things depending on which binary you invoked. This module is
the single source of truth they all import.

Consumers whose own package is named `cli` import it aliased:

```go
import protocolcli "go.putnami.dev/protocol/cli"
```

## Surface

| Symbol | Purpose |
|--------|---------|
| `ExitSuccess/Failure/Usage/Auth/API/Signal` | Exit-code taxonomy (`0/1/2/3/4/130`) |
| `OutputMode` + `ResolveOutputMode` | `text \| json \| jsonl \| cloud-logging`; `--json` = `--output=json` |
| `ResultV2` / `NewResultV2` / `WriteResultV2` | The `--output=json` envelope — the one every command emits |
| `SessionStreamRecord`, `BoundedSessionEndRecord`, `MCPResult`, `SessionFile`, `SessionPlanFile` | The other version-2 machine documents, including JSONL `plan:end` previews and the opt-in bounded session terminal |
| `ReportFile` + `ReportMax*` | The run's bounded synthesis document (`report.json`) and the caps that bound it |
| `TaskIdentity` / `RunSummary` / `TaskRecord` | The typed task identity and the one run verdict every v2 surface reports |
| `TestCase` + `TestCaseMax*`, `BoundTestCases`, `BoundTestCasesWithin`, `TestCaseBatchMemberBytes`, `TruncateTestCaseOutput` | The `test:case` stream record's payload, its bounds, and the helpers a test producer bounds it with — [doc/02-result-v2.md](doc/02-result-v2.md#test-case-records) |
| `ValidateDocument`, `ValidateSessionStream` / `Violation` | Per-document and whole bounded-stream validation, mirrored in TypeScript and pinned by the cross-language corpus |
| `Result` / `ResultError` | The version-1 envelope, retained as a READ-ONLY shape for documents recorded by builds published before its removal — its constructor and writer are deleted |
| `Err*` sentinels, `Classify`, `ExitCodeForError`, `ErrorCode` | Error classification → exit code |
| `SessionReporterCommand`, `LogReporterCommand` and their `*Env`/`*TokenEnv`, `SessionReportingArtifacts`, `SessionReportingPlanBytes`, `SessionReportingChunk`, `SessionReportingAck`, `SessionReportingHandshake`, `SessionReportingHandshakeResult` | The native reporting capabilities (session reporter, log reporter), their chunk/ACK wire, and the v2 handshake that hands a reporter the run credential of a hosted run — [doc/04-session-reporting.md](doc/04-session-reporting.md) |
| `SessionSubscribersFile`, `NewSessionSubscriberEvidence`, `ParseSessionSubscribersFile` | Per-subscriber delivery evidence (`subscribers.json`) for a session's event stream — [doc/05-session-subscribers.md](doc/05-session-subscribers.md) |
| `ReservedGlobalFlags`, `IsReservedGlobalFlag`, `LookupReservedGlobalFlag`, `ValidateNoReservedShadow` | The reserved global-flag registry — flags every surface handles and no extension may redefine |
| `CommandSurface`, `NewCommandSurface`, `MarshalCommandSurface`, `ParseCommandSurface`, `ErrUnknownCommandSurfaceVersion`, `IncompatibleCommandChanges` | The command-surface document a CLI commits, and the comparison that finds an incompatible change to its commands and flags — [doc/06-command-surface.md](doc/06-command-surface.md) |

See [doc/01-contract.md](doc/01-contract.md) for the full contract.

Both envelopes are published as JSON schemas so non-Go surfaces can bind to the
same shapes: [schemas/result-v2.json](schemas/result-v2.json) (`$id`
`https://putnami.dev/schemas/putnami-cli-result-v2.json`) is the emitted one, and
[schemas/result.json](schemas/result.json) (`$id`
`https://putnami.dev/schemas/putnami-cli-result.json`) documents the retired
version-1 shape for readers. A drift test keeps each schema and its Go structs
in lock-step.

## Machine contract version 2

[doc/02-result-v2.md](doc/02-result-v2.md) specifies the versioned machine
contract: `protocolVersion` on all four machine surfaces (`--output=json`,
`--output=jsonl`, MCP, session files), a typed task identity, and the settled
rules for **unified success** (strict — a reused failure fails the run) and
**abort precedence** (`aborted` > `failure` > `success`, with the envelope and
the process exit code agreeing). A plan-only machine invocation reports
`ResultV2.plan` in JSON or one `plan:end` record in JSONL; neither claims a run
or persisted session. It is published as
[schemas/result-v2.json](schemas/result-v2.json).

Version 2 is now the **only** machine contract: the version-1 emitters are
deleted, so every document the CLI writes carries `protocolVersion: 2` and a
document without the member can only come from a build published before the
removal. The v1 `NewResult`/`WriteResult` pair went with them, so this module
can no longer WRITE a versionless document at all. The field-by-field mapping,
the retained reader paths, and the rollback (pin an older published build —
there is no environment variable) are in
[doc/02-result-v2.md § Migrating from version 1](doc/02-result-v2.md#migrating-from-version-1).
The decision itself is recorded in
[ADR 0002 of `@putnami/cli`](../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md).

`ValidateDocument` implements the contract, `@putnami/cli-protocol` mirrors it in
TypeScript, and [conformance/manifest.json](conformance/manifest.json) is the one
corpus both runtimes execute — so Go/TypeScript drift is a test failure rather
than a wire that quietly means two things.

## Bounded JSONL output

[doc/04-machine-output.md](doc/04-machine-output.md) specifies the opt-in bounded
stream profile: fixed normal/verbose totals, hard ordinary/failure/final
partitions, exact split elision counts, sanitization before persistence and
measurement, and the complete retained `events.jsonl` artifact. Legacy
`SessionStreamRecord` lines remain valid; only a `BoundedSessionEndRecord` plus a
whole live/artifact pair passing `ValidateSessionStream` makes the bounded
promise. This module defines that opt-in wire; the CLI producer remains legacy
until the dedicated producer-adoption task enables it.

## The command surface

[doc/06-command-surface.md](doc/06-command-surface.md) specifies the
command-surface document: the commands, flags and positionals a CLI's users
type, sorted and free of help prose, at `protocolVersion: 1`. A reader reads
every version up to its own, and reports a later one as
`ErrUnknownCommandSurfaceVersion` rather than as a malformed document.
`IncompatibleCommandChanges` compares two of them and names each change that
can break an invocation that used to work, such as a removed flag or a value
dropped from a closed list. It is published as
[schemas/command-surface.json](schemas/command-surface.json) (`$id`
`https://putnami.dev/schemas/putnami-cli-command-surface.json`). Go is its only
runtime.

## The session report

[doc/03-report.md](doc/03-report.md) specifies `reportFile` — the run's
contracted *bilan* (`report.json`). It carries the verdict,
a per-command synthesis, a **bounded** per-job synthesis, and typed cache and
scheduler subsets, so a consumer picks up a 26 KB document instead of scraping a
539 KB session. Three properties define it: absent is not zero (a fact the run
could not measure is omitted), it is a projection and never an input (nothing in
it affects pass/fail or cache keys), and it is bounded by contract — `jobs` ≤ 64,
diagnostics ≤ 16 per job, messages ≤ 1024 UTF-8 bytes, with everything the
ceilings drop **counted** rather than lost.

## Producers and consumers

**Producers** — the surfaces that emit these documents:

| Producer | Surface |
|----------|---------|
| `tooling/cli/internal/output`, `.../machine` | `--output=json`, `--output=jsonl`, session and plan files, `report.json` |
| `tooling/cli/internal/mcp` | The MCP `run_jobs` / `plan_jobs` results |
| `tooling/cli/internal/telemetry`, `.../watch`, `.../engine`, `.../jobs` | Run summaries, task records and stream records |
| `go/extension/internal/jobs/test`, `typescript/extension/cmd/putnami-ts`, `python/extension/internal/jobs` | The bounded `TestCase` list a test task hands the CLI, which the CLI writes as `test:case` records |
| `tooling/cli/internal/commandmeta` | The CLI's command surface, committed as `tooling/cli/command-surface.json` |

**Consumers**

| Consumer | Use |
|----------|-----|
| `tooling/cli/internal/{cli,clibin,cmderr,commands,extension,launch,lockfile,workspace_state}` | Exit-code classification, output-mode resolution, reserved flags, and reading recorded sessions |
| `tooling/extension-sdk/{cli,manifest,runtimeinfo}` | The exit-code taxonomy and reserved global-flag registry every extension binary must honour |
| `go/extension/internal/jobs/pkg`, `typescript/extension/internal/pkg` | The package-time contract stamp |
| `go/extension/internal/jobs/apicheck` | Compares a stable project's command surface with its line's last tag in `validate` |
| [`protocols/extension`](../extension/README.md) | Negotiates a manifest's `cliContract` against `CurrentContract` |
| [`protocols/job`](../job/README.md) | Shares the typed task identity |
| [`protocols/ci`](../ci/README.md) | Carries the typed task identity in every ChangePlan and ImpactPlan task |
| `@putnami/cli-protocol` (`typescript/framework/cli-protocol`) | The TypeScript twin of the validators |

## Versioning and compatibility

Two independent versions live here, and conflating them is the classic mistake:

| Version | Meaning | Value |
|---------|---------|-------|
| `ResultProtocolVersion` | The **machine document** contract stamped as `protocolVersion` on every emitted document | `2` |
| `CurrentContract` | The **CLI ↔ extension** contract an extension manifest is stamped against | `4` |

Compatibility rules:

- **`protocolVersion` presence is the whole detection story.** It is on every v2
  document and absent from every v1 one, so a consumer that must handle both
  branches on `typeof doc.protocolVersion === 'number'` and needs no other
  probe.
- **Every v2 document is closed** (`additionalProperties: false`), so a new
  member is a schema change reviewed on both sides, never a silently tolerated
  extra.
- **Additive members stay additive.** `executions[]`, `environment`, `run.cpu`
  and `preparation` were added without renaming or re-typing anything an older
  reader knew, so a session written by a current CLI still parses as one written
  before it.
- **Version 1 is read-only.** Its Go type and schema are retained for documents
  already recorded; nothing in this module can write one. Rolling back means
  pinning an older published build — there is no environment variable.
- **The contract ladder has two outcomes.** An extension manifest either
  declares exactly `CurrentContract`, or it does not load. Adaptation of older
  manifests is deleted rather than deprecated.

## Schemas, fixtures, and the cross-language corpus

- Schemas: [`schemas/result-v2.json`](schemas/result-v2.json) (`$id`
  `https://putnami.dev/schemas/putnami-cli-result-v2.json`, the emitted shape)
  and [`schemas/result.json`](schemas/result.json) (`$id`
  `https://putnami.dev/schemas/putnami-cli-result.json`, the retired v1 shape
  kept for readers). `drift_test.go` and `drift_v2_test.go` hold each schema and
  its Go structs in lock-step.
- Command surface: [`schemas/command-surface.json`](schemas/command-surface.json),
  held against its Go types by `command_surface_test.go`, with a Go-only
  corpus in [`testdata/command-surface/`](testdata/command-surface/documents.json).
- Conformance corpus: [`conformance/manifest.json`](conformance/manifest.json)
  and [`conformance/pack.json`](conformance/pack.json), documented in
  [`conformance/README.md`](conformance/README.md). Each case embeds the exact
  document bytes and an `accept`/`reject` expectation, and **both** the Go
  validator and `@putnami/cli-protocol` execute the same file — so a Go/TypeScript
  disagreement is a test failure naming the disagreement, not a wire that quietly
  means two things.

## Support status, owner, and evidence

| | |
|---|---|
| **Status** | `stable` — see the `go.putnami.dev/protocol/cli` entry in the workspace-root [`putnami.support.json`](../../putnami.support.json), which is the only authority for this value |
| **Owner** | The `protocols` scope (`protocols/putnami.json`). This module is the sole authority for the exit-code taxonomy, the output modes, the reserved global-flag registry, and the machine documents; no CLI surface or SDK may define its own. |

Evidence behind `stable`:

- a pinned `ResultProtocolVersion` and a pinned `CurrentContract`, each with a
  stated compatibility rule and an explicit v1 → v2 migration section;
- two published JSON Schemas, each held against its Go structs by a drift test;
- a **cross-language** conformance corpus executed by two independent runtimes,
  which is the strongest evidence available in this repository;
- over twenty consumer packages across the CLI, the extension SDK, and three
  language extensions, plus two sibling protocols that build on its types.

## Product feature and durable decisions

**No user-facing feature is declared for this module, deliberately.** The user
outcomes it serves — a CLI whose exit codes mean something, and machine output an
agent can parse — are delivered by `@putnami/cli` itself, which is what the
support catalog classifies as a package. A feature per protocol module would
create one artificial product identity per technical boundary, which the
[feature protocol's authoring boundary](../features/README.md) rules out. With no
feature to detail there is no spec, since a spec details exactly one
already-authored feature and never mints one.

Durable decisions (both scope `protocols/cli` explicitly):

- [ADR 0001 — CLI foundation boundaries: catalog, result, engine](../../tooling/cli/doc/adr/0001-cli-foundation-boundaries.md)
- [ADR 0002 — CLI vNext: one contract with four surfaces, and the ratchets that hold it](../../tooling/cli/doc/adr/0002-cli-vnext-contracts.md)
