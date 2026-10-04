# Native session reporting

Select an installed extension implementing `session-reporter`:

```sh
PUTNAMI_SESSION_REPORTER=@example/reporter putnami lint,test,build
```

The engine reports the recorded plan and persisted events while the graph
runs, then the terminal session, including failed and canceled work. Local and hosted execution use the
same capability. A trusted launcher may supply `PUTNAMI_SESSION_REPORTER_TOKEN`;
the authoritative CLI captures it before workspace setup/hooks/tasks and passes
it only to the selected provider process. Do not put it in repository commands.
A hosted run (`--credential-fd`) ignores the token instead: it removes it from
the environment with a warning, and hands the reporter the run credential over
the protocol. See [Hosted runs](#hosted-runs).

A selected but unavailable provider produces a diagnostic and retains the
session; an unset selector leaves reporting disabled. Reporting failure never
changes the graph verdict. Provider stderr and arbitrary error text are not
forwarded. `.putnami/sessions/<id>/reporting.json` records completion and the
bounded pending checkpoint alongside the ordinary artifacts.

The reporter is a declared subscriber of the session's event stream
(`events.jsonl`) and never slows a task. While the graph runs it batches: an
events chunk leaves once a full 64 KiB frame is committed, or 10 seconds after the
previous events chunk. The first events chunk waits 10 seconds from the reporter's start.
A long run therefore sends a number of chunks bounded by its duration, not one per
record. When the graph ends, the reporter sends the rest at once. When it stops, `.putnami/sessions/<id>/subscribers.json` records its evidence:
`delivered`, `partial` or `lost`, the last acknowledged position, and how many
records it never acknowledged. `putnami sessions inspect <id>` prints it. A replay
rewrites only the entries of the reporters it replays. See the
[evidence contract](../../../protocols/cli/doc/05-session-subscribers.md) and
[ADR 0033](adr/0033-native-session-reporting.md).

## Recorded plan

Before any event, the session reporter sends the session's recorded plan,
`.putnami/sessions/<id>/plan.json`: the commands, and the planned tasks with
their edges. It leaves as soon as the reporter starts, so the receiver has it
before the first `task:start` record. The CLI sends the persisted bytes
unchanged and adds no redaction, as for `session.json`. A `plan.json` above
16 MiB is not sent.

The plan is best-effort. The reporter skips it, records why in
`reporting.json` (`planOmitted`), and still delivers the events and the
session:

| `planOmitted` | Why |
| --- | --- |
| `absent` | The session has no `plan.json`. |
| `too_large` | `plan.json` is above 16 MiB. |
| `refused` | The receiver refused a `plan.json` chunk, without retry or on the last attempt. |
| `undeliverable` | Every attempt to send a `plan.json` chunk failed in transport, for example a receiver that exits. |
| `late` | A checkpoint written by an earlier CLI already sent events or the session. |

An omitted plan prints no diagnostic and changes neither the exit code nor
`subscribers.json`. Replay never sends a plan the run omitted. The log reporter
never receives `plan.json`. On a hosted run, a receiver that exits at
`plan.json` instead of refusing it is started again after repository code ran,
gets no credential, and the session's delivery fails (see
[Hosted runs](#hosted-runs)): upgrade a hosted receiver to accept or refuse
`plan.json` before you upgrade the CLI.

## Log reporter

Select an installed extension implementing `log-reporter` to receive only the
live event stream:

```sh
PUTNAMI_LOG_REPORTER=@example/logs putnami lint,test,build
```

The log reporter receives `events.jsonl` through the same wire and never
receives `session.json` or `plan.json`. A live chunk leaves once a full 64 KiB frame is
committed, or 2 seconds after the previous events chunk. When the graph ends, it
sends the rest at once and closes the stream. `PUTNAMI_LOG_REPORTER_TOKEN` is
its optional credential. The CLI captures it like the session reporter's token
and passes it only to the log reporter's provider process.

The two reporters are independent. Select either, both or neither; one
extension may implement both. Each has its own provider process, its own
checkpoint (`log-reporting.json` for the log reporter) and its own
`subscribers.json` entry. A log reporter that is unavailable, refuses a chunk
or crashes produces one diagnostic, and changes neither the session reporter's
delivery nor the exit code. The reverse also holds. See
[ADR 0033](adr/0033-native-session-reporting.md).

## Hosted runs

A hosted run (`--credential-fd`, see
[ADR 0055](adr/0055-run-credentials-stay-out-of-repository-processes.md))
keeps every reporter token out of every process environment:

1. At start, the CLI removes `PUTNAMI_SESSION_REPORTER_TOKEN` and
   `PUTNAMI_LOG_REPORTER_TOKEN` from its environment, and prints one line for
   each that held a value. It never reads them on that run.
2. Before its first hook, install or job, it starts each selected reporter and
   hands it the run credential over the protocol's version 2 handshake. The
   session's delivery reuses that process.
3. A reporter that does not accept the handshake (a version 1 reporter), or
   whose command is not its extension's native runtime, starts without a
   credential and without a token. The CLI prints one diagnostic for it.
   Each handshake answer has its own five-second limit, and the CLI starts
   the reporters one after another. A reporter answers two lines, so the
   handshake can hold the first hook for up to ten seconds per reporter, and
   up to twenty with both reporters selected. A reporter that ignores the
   handshake instead of exiting costs five seconds.
4. A reporter that must start again after repository code ran gets no
   credential: its delivery fails with a custody diagnostic. What it did not
   deliver stays pending in the session's checkpoint (`reporting.json` or
   `log-reporting.json`), and `subscribers.json` records it. The hosted run
   does not replay it. A later `putnami sessions replay` is a separate
   invocation and needs a reporter credential of its own.
5. `--watch`, and `serve`, which always watches, refuse `--credential-fd` with
   a usage error before anything starts: each watch iteration starts after
   repository code ran, when no reporter can receive the credential.

A run without `--credential-fd` is unchanged: no handshake, and each token
reaches only its own reporter's environment.

## Replay

Resume incomplete delivery with fresh execution-scoped credentials:

```sh
PUTNAMI_SESSION_REPORTER=@example/reporter PUTNAMI_LOG_REPORTER=@example/logs putnami sessions replay --session 20260914-120000-abc123
```

Replay resumes each selected reporter whose entry is not `delivered`, from its
own acknowledged position, and leaves a delivered reporter untouched. At least
one reporter must be selected. Each reporter has its own finalization limits.
Replay uses the original sequences, offsets and pending identity. It requires a
valid finalized v2 session with the exact matching id (`latest` is refused),
performs normal reporter discovery/runtime preparation, and runs no workload
DAG or workspace lifecycle hooks. It refuses a changed extension on an existing
checkpoint. The provider must also refuse a changed destination/execution binding
behind the same extension name.

Transport uses chunks of at most 64 KiB, five-second RPC deadlines and at most
three attempts. Each reporter's finalization, and its replay, stops after thirty
seconds without an acknowledged chunk, or five minutes after it began, whichever
comes first: a reporter that keeps acknowledging delivers its backlog and its
final markers, and a stuck one stops at the latest thirty seconds after its
last acknowledgement. The diagnostic names the limit, and keeps the replay
hint:

```text
putnami: session reporting incomplete: reporting budget exhausted: no chunk acknowledged for 30s; replay with putnami sessions replay --session <id>
putnami: log reporting incomplete: reporting budget exhausted: finalization reached its 5m0s cap; replay with putnami sessions replay --session <id>
```

Replay bounds each reporter's setup to thirty seconds before its delivery
starts. Canceled graphs drain for two seconds so existing shutdown cleanup can
run. A large or unavailable receiver can require another replay. Checkpoint
reads are bounded to 192 KiB; replay refuses session documents above 16 MiB
with the original artifacts intact.
Deadlines bound subprocess waits and transport, not arbitrary local filesystem I/O.

Pending reporting, for either reporter, has priority over ordinary history
within `sessions.keep` (default 20). If pending sessions themselves exceed the limit, the oldest
inactive pending session expires with a diagnostic: its replay is unavailable.
Active reporters/replays hold a lock and can temporarily exceed the limit.
Replay or export before discarding the worktree. A missing finalized document
after engine loss is never turned into a terminal result; the supervisor owns
that interruption fact. Pre-execution failures and lifecycle runs that create
no session remain outside this capability.

Selecting an extension name does not establish trust in its manifest/executable.
Environment withholding is not file/same-UID isolation or a metadata/network
sandbox. Hosted credentials require those independent controls before activation.

The [wire contract](../../../protocols/cli/doc/04-session-reporting.md) includes
Go/TypeScript types, a shared corpus, ACK semantics and manifest declaration.

Component proof from this source workspace:

```sh
./putnamiw test --projects @putnami/cli --run '^(TestNativeSessionReporting|TestLogReporting|TestSessionStream)' --no-cache --no-enforce-coverage
```

This uses `Engine.Run`, normal discovery, real scheduler/task processes and a
real provider subprocess. A local directory replaces only the remote durable
receiver. The live workload cannot finish until the provider receives
`task:start`; failure, cancellation, outage, replay and explicit environment
withholding are separate assertions. The log reporter tests run a second real
provider beside the first and prove selection, failure isolation, token custody
and replay of only the incomplete reporter. Placement coverage verifies that absent
runner providers still report one local fallback with requested/actual provenance,
while an installed runner is rejected before runtime preparation or reporting.
This proves no hosted deployment or metadata isolation.
