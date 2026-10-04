# ADR 0033 — The engine owns session reporting through one event stream

- **Status**: accepted
- **Scope**: `tooling/cli/internal/engine`, `internal/sessionstream`,
  `internal/sessionreporter`, `protocols/cli` (session reporting v1 and v2,
  session subscribers)

## Context

Only the native engine observes the whole graph lifecycle and owns the
persisted session. An after-build uploader cannot stream live work and cannot
describe a failed or canceled graph; a report task with successful
prerequisites has the same defect. Extensions need two kinds of delivery: the
full session (events, then the terminal `session.json`) and the live event
stream alone, such as a log view. Neither may slow a task or change a verdict.

## Decision

### One event stream, declared subscribers

A recorded session has one event stream, its `events.jsonl`. The session
recorder is its only producer (`internal/sessionstream`). It appends one
LF-terminated record per write, never rewrites a byte, and adds no member to a
record. A position is a byte offset; its record count is the number of LF bytes
before it.

- `Subscribe(name, from)` starts at a byte offset. Every byte before it counts
  as acknowledged, so a durable cursor resumes and a reader passes 0.
- The producer wakes subscribers with a non-blocking send on a one-slot
  channel. A slow or dead subscriber never delays a task.
- A subscriber reads by offset from the file, never past what the producer
  committed. `Ack` only moves forward; there is no reset.
- Closing the stream is the terminal `final` marker. A final acknowledgement
  requires it.
- After a live subscriber stops, `subscribers.json` beside `session.json`
  records its evidence: `delivered`, `partial` or `lost`, the last acknowledged
  position and the records never acknowledged. It cannot live in
  `session.json`, which the session reporter delivers before its own evidence
  exists. The document is declared in
  [`protocols/cli`](../../../../protocols/cli/doc/05-session-subscribers.md).

`sessions inspect` reads the finalized stream from 0 and records no evidence. Two consumers are deliberately not subscribers:

- **The telemetry observer.** It fires for runs that record no session
  (`--dry-run`, the outer `--watch` loop) and before the session exists.
- **The failed-task record** ([ADR 0030](0030-a-failed-task-is-cached-until-its-inputs-change.md)).
  It needs the cache key, must be forgotten synchronously on success, and
  serves results before execution, which an observation seam must never do.

### Reporting capabilities are rows of one table

A reporting capability is one row of `Capabilities()` in
`internal/sessionreporter`:

| Capability | Command | Selector | Token | Artifacts | Live chunk leaves at | Checkpoint, lock |
| --- | --- | --- | --- | --- | --- | --- |
| Session reporter | `session-reporter` | `PUTNAMI_SESSION_REPORTER` | `PUTNAMI_SESSION_REPORTER_TOKEN` | `events.jsonl`, then `session.json` | 64 KiB or 10 s | `reporting.json`, `reporting.lock` |
| Log reporter | `log-reporter` | `PUTNAMI_LOG_REPORTER` | `PUTNAMI_LOG_REPORTER_TOKEN` | `events.jsonl` | 64 KiB or 2 s | `log-reporting.json`, `log-reporting.lock` |

- Command, environment and artifact names belong to the protocol
  (`go.putnami.dev/protocol/cli`, `@putnami/cli-protocol`). The wire is session
  reporting v1 ([contract](../../../../protocols/cli/doc/04-session-reporting.md)),
  opened on a hosted run by the v2 handshake that hands the run credential.
- The engine runs each selected capability as its own subscriber, provider
  subprocess, checkpoint, lock and `subscribers.json` entry. It prepares
  providers one after another, so one extension serving both commands never
  prepares its runtime twice at once. They drain concurrently.
- A live chunk waits for a full frame or the capability's interval, then drains
  at once when the graph ends. A capability acknowledges a position only after
  its checkpoint is durable.
- The session reporter closes `session.json` before its events final marker,
  so a consumer can derive event metrics from the final resource accounting.
  The log reporter's final marker follows graph termination.
- A failing capability (unavailable extension, refused chunk, crash, exhausted
  budget) produces at most one diagnostic of its own. It never changes the
  other capability, the graph verdict or the exit code.
- A new capability is a reviewed table row, protocol constants in both
  runtimes and a checkpoint entry in session retention. Extensions cannot
  declare subscriber names: each name widens what the CLI captures and
  withholds.

### Durable delivery

- One atomic checkpoint per capability beside the retained session holds the
  ACK cursors and the exact pending frame. Provider identity, sequence, byte
  offset, SHA-256 and final identity survive retry and replay.
- ACK means durable acceptance. An identical duplicate succeeds; conflicting
  bytes fail. A provider binds the destination to the execution identity and
  refuses a changed destination. Core refuses a different extension on an
  existing cursor. There is no cursor reset.
- RPC reads and writes share a deadline, retries are finite, and each
  capability's finalization and replay stop after 30 s without an
  acknowledged chunk, or 5 min after they began. A canceled graph drains for
  at most 2 s, inside the CLI's 10 s forced shutdown.
- `putnami sessions replay` resumes each selected capability whose evidence is
  not `delivered`, from its own cursor, rewrites only those entries, and runs
  no workload. It fails when no capability is selected.
- Session retention treats a session as pending while any capability's
  checkpoint is incomplete. Pending state has priority within `sessions.keep`
  but no unlimited exemption; expiry is diagnosed. A session whose capability
  lock is held is never pruned.

### Credential custody

The CLI captures every selector and token after the authoritative pin relaunch
and before extension setup, hooks, runtime preparation and tasks, and removes
them from its environment. Each provider subprocess receives only its own
token; no task, hook or other provider does. Tokens never enter checkpoints or
frames. Provider stderr is discarded and provider text never enters
diagnostics. These are environment-custody guarantees only: choosing an
extension name does not prove its manifest or runtime trustworthy, and this
decision adds no same-principal isolation and no metadata or network fence.

A hosted run (`--credential-fd`,
[ADR 0055](0055-run-credentials-stay-out-of-repository-processes.md)) ignores
both tokens: the CLI removes them with a warning and places neither in any
environment. It hands each reporter the run credential over session reporting
v2 instead: `initialize`, which carries no credential, then, only after the
reporter accepts it, `authenticate`. A version 1 reporter rejects
`initialize`, so it starts without a credential or a token, with a
diagnostic. The run starts every selected reporter before its first
repository code, because custody hands nothing to a process started later; a
reporter restarted after repository code gets no credential and its delivery
fails. A run without the flag sends no handshake, and its frames are
byte-identical to version 1.

### Boundaries

The consumer owns interpretation and downstream effects. Admission, queued
work and executor loss stay supervisor facts. A hard-killed engine cannot
finalize a session, and the reporter never fabricates one. Runs that create no
session and failures before execution are outside reporting. Reporting is
separate from the opt-in telemetry observer and adds no vendor report shape,
service or dependency. Unset selectors leave behavior unchanged.

## Alternatives rejected

- **One selector naming a set of reporters.** Every token would reach every
  provider.
- **A log reporter that also receives `session.json`.** It duplicates the
  session reporter and ties a live consumer to finalization order.
- **One provider process fanning out to both consumers.** One crash loses both
  deliveries.
- **A polling reporter.** Subscribers wait on the wake signal and batch by
  frame or interval, so the chunk count follows run duration, not ACK round
  trips.

## Consequences

- A remote placement ships the same stream a local run records, once and in
  order, through the same contract.
- A reporter that cannot start is still declared: its evidence says `lost`.
- Operator usage: [16-session-reporting.md](../16-session-reporting.md).
