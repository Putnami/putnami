# Native session reporting

Select an installed extension implementing `session-reporter`:

```sh
PUTNAMI_SESSION_REPORTER=@example/reporter putnami lint,test,build
```

The engine reports the recorded plan and a trimmed view of the persisted
events while the graph runs (see [Trimmed events](#trimmed-events)), then the
terminal session, including failed and canceled work, and last the spec gate's
record. Local and
hosted execution use the same capability. A trusted launcher may supply
`PUTNAMI_SESSION_REPORTER_TOKEN`; the CLI captures it before it fetches a
pinned CLI, and before workspace setup/hooks/tasks, hands it to the pinned
CLI it execs and otherwise passes it only to the selected provider
process. Do not put it in repository commands. A hosted run
(`--credential-fd`) ignores the token instead: it removes it from the
environment with a warning, and hands the reporter the run credential over the
protocol. See [Hosted runs](#hosted-runs).

A selected but unavailable provider produces a diagnostic and retains the
session; an unset selector leaves reporting disabled. Reporting failure never
changes the graph verdict. Provider stderr and arbitrary error text are not
forwarded. `.putnami/sessions/<id>/reporting.json` records completion and the
bounded pending checkpoint alongside the ordinary artifacts.

The reporter is a declared subscriber of the session's event stream
(`events.jsonl`) and never slows a task. While the graph runs it batches: an
events chunk leaves once a full frame of the view is ready, 64 KiB or the larger
cap the reporter acknowledged (see [Larger frames](#larger-frames)), or 10 seconds
after the previous events chunk. The first events chunk waits 10 seconds from the reporter's start.
A long run therefore sends a number of chunks bounded by its duration, not one per
record. When the graph ends, the reporter sends the rest at once, or, for a
job run from the terminal, leaves it to a background replay (see
[Delivery after the run returns](#delivery-after-the-run-returns)). When it stops, `.putnami/sessions/<id>/subscribers.json` records its evidence:
`delivered`, `partial` or `lost`, the last acknowledged position, and how many
records it never acknowledged. `putnami sessions inspect <id>` prints it. A replay
rewrites only the entries of the reporters it replays. See the
[evidence contract](../../../protocols/cli/doc/05-session-subscribers.md) and
[ADR 0033](adr/0033-native-session-reporting.md).

## Trimmed events

The session reporter's `events.jsonl` frames carry a trimmed view of the
stream, not its bytes
([ADR 0083](adr/0083-the-session-reporter-uploads-a-trimmed-view-of-the-event-stream.md)).
`events.jsonl` on disk keeps every record but those a reused task replays from
its cache entry, which the view puts back ([Reused tasks](#reused-tasks)), and
the [log reporter](#log-reporter) receives every record, untrimmed. The view:

1. Drops every `progress` event.
2. Holds every `task:event` of a task from its first `debug` log line to its
   `task:end`. At `task:end`, the held records leave in stream order, just
   before it. A `success` or `skipped` end drops the held `debug` lines and
   sends the others; any other end sends them all.
3. Sends a batch line that `batchProjectId` or `batchProjectIds` attributes
   to several members once, at the first member end that keeps it. A shared
   `debug` line is dropped when every member passes.
4. Sends what is still held at `session:end` and at the stream's final
   marker, then a torn last line, as it is.
5. Past 1,000,000 held records, sends the records of the task that holds the
   most, and sends that task live from then on. A tie goes to the task whose
   first held record comes first, then to the task whose `identity.key` sorts
   first. A record that names a task sent live leaves live.

Every `task:start`, `task:end`, `test:case` and `session:end` record leaves,
and a line that is not a JSON record leaves as it is. A task that logs `debug`
lines sends its `info`, `warn` and diagnostic events at its end, not live.
Held records do not fill a frame: the batching rule counts only the view bytes
that are ready.

The offsets and sequences of the session reporter's `events.jsonl` chunks are
view offsets: they count the bytes the view sent, not the bytes of the file.
The log reporter's are raw offsets, but in a session that records
[reused tasks](#reused-tasks) by reference. `reporting.json` is at version 2,
or 4 in a session that records reused tasks by reference: its `events` cursor
is the view cursor, and `eventsRaw` is the raw offset of `events.jsonl` that
`subscribers.json` acknowledges. Once the receiver
acknowledges the view's final marker, `subscribers.json` reads `delivered`.
After an engine restart or a replay, the CLI rebuilds the view from the start
of `events.jsonl`, so a resent sequence keeps its bytes.

A session whose `reporting.json` is at version 1 and already sent an
`events.jsonl` chunk keeps raw framing to its end; one that sent none moves to
version 2, or 4. A session on [task packages](#task-packages) is at version 3,
or 5. A CLI that predates version 2 refuses a version 2 checkpoint, one that
predates packages a version 3 checkpoint, and one that predates
[reused tasks](#reused-tasks) a version 4, 5 or 6 checkpoint: replay such a
session with this CLI or a later one. The log reporter's
`log-reporting.json` is at version 1, or 6 in a session that records reused
tasks by reference.

## Reused tasks

A task that a task-owned entry of the local store served (a local hit, a
remote hit, a coalesced restore) is recorded in `events.jsonl` as its
`task:start`, the records the hit added in this run, and its `task:end`, whose
`cacheReference` names the entry
([ADR 0137](adr/0137-a-cached-task-is-recorded-by-its-cache-entry.md)). The
events the entry holds and the task's `test:case` records stay in the entry.
Such a session says so in `references.json`, which names the store its
references resolve in, and every reader rebuilds the records right before the
`task:end`, which it then reads without its reference:

- the session reporter's view, at checkpoint version 4;
- the session reporter's [task packages](#task-packages), at version 5, whose
  header names the rebuilt records in `rebuilt`: only a receiver that offers
  `packagesRebuilt` gets them, and the session stays on frames otherwise;
- the [log reporter](#log-reporter), at version 6, which sends every record of
  the stream, untrimmed;
- `putnami sessions inspect`.

The receiver reads the same records as for a task recorded whole; the events
the hit added in this run come before the rebuilt ones. A reference names the
projection that rebuilds its records, their counts and their SHA-256. A CLI
whose projection differs, or whose rebuilt records differ in count or hash to
another digest from a result whose bytes are the producer's, sends none: delivery fails with `reporting cannot rebuild a reused task's records`,
which names the projection, and the session waits for a CLI that rebuilds it.
`sessions inspect` then shows the task's `task:end` as recorded, with a note
that it is not resolvable by this CLI.

When the session ends, the CLI keeps the `result.json` of every entry it
references in the store, under `pins/objects/`, hard-linked from the entry or
copied where the file system links nothing, and records which in a pin of
the session. The entries themselves stay under ordinary collection; each
collection deletes the pins of sessions whose directory is gone and the
results no pin names. A pin names the session by its absolute path: a session
moved or reached through another path loses its pin at the next collection.

A session writes every record when it runs with `--output=jsonl`, executes a
bound portable request or runs inside one (the portable run tells the CLIs
its tasks start through `PUTNAMI_RECORD_SESSIONS_WHOLE`), verifies the cache
in another store, or runs with `--no-cache`. A reference whose result the
store no longer holds, after `putnami cache clean`, puts nothing back on any
surface: the `task:end` leaves as recorded, its `cacheReference` included, so
a receiver sees which task's records are missing.

## Recorded plan

Before any event, the session reporter sends the session's recorded plan,
`.putnami/sessions/<id>/plan.json`: the commands, and the planned tasks with
their edges. It waits for no batching interval, and every other frame
follows it, so the receiver has it before the first `task:start` record. A
task may start before the plan leaves. The CLI sends the persisted bytes
unchanged and adds no redaction, as for `session.json`. A `plan.json` above
16 MiB is not sent.

The plan is best-effort. The reporter skips it, records why in
`reporting.json` (`planOmitted`), and still delivers the events and the
session:

| `planOmitted` | Why |
| --- | --- |
| `absent` | The session has no `plan.json`. |
| `too_large` | `plan.json` is above 16 MiB. |
| `unreadable` | `plan.json` cannot be read: it is not a regular file, a read fails, or it was removed or truncated after its first chunk. |
| `refused` | The receiver refused a `plan.json` chunk without retry. |
| `undeliverable` | The last attempt to send a `plan.json` chunk failed in transport, for example a receiver that exits. |
| `late` | A checkpoint written by an earlier CLI already sent events or the session. |

An omitted plan prints no diagnostic and changes neither the exit code nor
`subscribers.json`. Replay never sends a plan the run omitted. A receiver that
refuses a `plan.json` chunk with retry on its last attempt does not omit it: the
delivery fails as for any chunk, and replay sends the plan first. The log
reporter never receives `plan.json`. On a hosted run, a receiver that exits at
`plan.json` instead of refusing it is started again after repository code ran,
gets no credential, and the session's delivery fails (see
[Hosted runs](#hosted-runs)): upgrade a hosted receiver to accept or refuse
`plan.json` before you upgrade the CLI.

## Spec gate record

Once the receiver acknowledges the events final marker, the session reporter
sends the spec gate's record, `.putnami/sessions/<id>/spec-verification.json`:
what each requirement of each judged spec resolved to. A record with no group
and no diagnostic means the gate ran and no selected project states a
requirement; a session without the file evaluated nothing. The engine writes it
before reporting ends, so it is the last artifact the receiver gets. The CLI
sends the persisted bytes unchanged. A record above 8 MiB is not sent.

The record is best-effort. The reporter skips it, records why in
`reporting.json` (`specVerificationOmitted`), and the delivery stays complete:

| `specVerificationOmitted` | Why |
| --- | --- |
| `absent` | The session has no `spec-verification.json`. |
| `too_large` | `spec-verification.json` is above 8 MiB. |
| `unreadable` | `spec-verification.json` cannot be read: it is not a regular file, a read fails, or it was removed or truncated after its first chunk. |
| `refused` | The receiver refused a `spec-verification.json` chunk, with or without retry, or acknowledged another chunk. |
| `quit` | The reporter exited, closed its pipe or stopped answering at a `spec-verification.json` chunk, or could not start. |
| `late` | The finalization limits ended before the record was delivered, or a checkpoint written by an earlier CLI completed without it. |

An omitted record prints no diagnostic and changes neither the exit code nor
`subscribers.json`: the events final marker is acknowledged, so the evidence
already reads `delivered`, and no replay sends the record again. The CLI
never restarts a reporter for the record, on a hosted run or not. A receiver that
predates the record can refuse it or exit: the run omits it, and the rest of
the session's delivery is unchanged. The log reporter never receives
`spec-verification.json`.

## Log reporter

Select an installed extension implementing `log-reporter` to receive only the
live event stream:

```sh
PUTNAMI_LOG_REPORTER=@example/logs putnami lint,test,build
```

The log reporter receives the raw `events.jsonl`, at raw offsets, through the
same wire, never the trimmed view; in a session that records
[reused tasks](#reused-tasks) by reference, it receives every record with the
reused tasks' records put back, at offsets of those bytes. It never receives `plan.json`,
`session.json` or `spec-verification.json`. A live chunk leaves once a full 64 KiB frame is
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
5. `--watch`, and a command whose traits declare the `serve` workload, such as
   `serve`, which watches by default, refuse `--credential-fd` with a usage
   error before anything starts: each watch iteration starts after
   repository code ran, when no reporter can receive the credential.

A run without `--credential-fd` is unchanged: no handshake, and each token
reaches only its own reporter's environment.

## Delivery after the run returns

A job run from the terminal returns once its result is durable locally. When
it ends, the CLI stops each reporter at its checkpoint instead of waiting for
the upload, and starts `putnami sessions replay --session <id>` in the
background, in a session of its own, with the same selectors and tokens.
That replay sends the rest. The run also starts one for each of the three
newest other sessions that are finalized, not yet delivered, and that no
replay is delivering, and names them in one line:

```text
putnami: 1 earlier session(s) not delivered by their reporters; replaying them in the background (putnami sessions inspect <id>)
```

So a session that an interrupted replay, an offline machine, a sleep or a
logout left is delivered by a later run. A session killed before it was
finalized cannot be replayed. Each frame keeps its sequence, offset and bytes
across these replays, so the receiver stores it once. The background replay
writes nothing to the terminal: its outcome stays in the session's
checkpoint and evidence, which `putnami sessions inspect <id>` prints, and
the diagnostics below appear only on a run that waits for its delivery or on
`putnami sessions replay`.

The run waits for its delivery, as before, when it must deliver before it
exits: a hosted run (`--credential-fd`), a CI job (`CI`, `JENKINS_URL`,
`TF_BUILD` or `TEAMCITY_VERSION` set), a run whose session could not be
finalized, and every command that is not a job run from the terminal. Set
`CI=true` to keep the delivery in the foreground anywhere else, for example
in a container reclaimed as soon as its command ends; `CI` also selects
the CI defaults of the flags that declare one. See
[ADR 0115](adr/0115-a-local-run-hands-its-report-upload-to-a-detached-replay.md).

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

Transport uses chunks of at most 64 KiB, or of the larger cap a session
reporter acknowledged, five-second RPC deadlines and at most three attempts. Each reporter's finalization, and its replay, stops after thirty
seconds without an acknowledged chunk, or five minutes after it began, whichever
comes first: a reporter that keeps acknowledging delivers its backlog and its
final markers, and a stuck one stops at the latest thirty seconds after its
last acknowledgement. The diagnostic names the limit, and keeps the replay
hint:

```text
putnami: session reporting incomplete: reporting budget exhausted: no chunk acknowledged for 30s; replay with putnami sessions replay --session <id>
putnami: log reporting incomplete: reporting budget exhausted: finalization reached its 5m0s cap; replay with putnami sessions replay --session <id>
```

A reporter that refuses an `events.jsonl` or `session.json` chunk without
retry stops its delivery; a refused `plan.json` or `spec-verification.json` is
omitted instead, as described above. The diagnostic names the refused chunk
and the reporter's code, a lowercase identifier such as `unauthorized`, so you
can tell a refused credential from a chunk the receiver did not expect:

```text
putnami: session reporting incomplete: reporter refused events.jsonl chunk 1: unauthorized; replay with putnami sessions replay --session <id>
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

## Larger frames

The engine starts the session reporter with `PUTNAMI_SESSION_REPORTING_FRAMING=3`,
which says that it reads a cap on each acknowledgement. A reporter whose
receiver advertises one (`X-Putnami-Session-Contract: 2` and
`X-Putnami-Session-Chunk-Bytes`, up to 4 MiB) names it as `chunkBytes`, and the
engine cuts each new frame at that cap instead of 64 KiB. A frame above
64 KiB is a version 3 frame. A receiver that advertises nothing gets the same
64 KiB frames as before. A version 3 frame a receiver does not take, which
it stored nothing of, is refused `frame_unsupported`, and the engine cuts it
again at 64 KiB at the same offset and sequence; a frame an earlier attempt
may have delivered is retried as it is. The log reporter keeps 64 KiB frames. The decision and
what the receiver changes are in
[ADR 0131](adr/0131-a-session-reporter-sends-larger-frames-once-its-receiver-advertises-them.md).

## Task packages

With a receiver that also advertises `X-Putnami-Session-Packages: 1`, the
engine names framing version 5 to the session reporter (version 4, which an
older engine names, says the same but for rebuilt records) and, once `plan.json`
has gone as frames, delivers the rest of the session as task packages: every
record of `events.jsonl`, byte for byte and with its offset, in one package
per task, sealed at the task's end. Whenever one of eight slots is free, the
packages sealed since leave in one put; `session.json` and
`spec-verification.json` follow in parts, then a final manifest. Nothing is
trimmed, and a replay sends only the packages not yet acknowledged. A
receiver that advertises no packages keeps the session on frames. The
decision and the receiver's contract are in
[ADR 0135](adr/0135-a-run-delivers-its-report-as-task-packages-sent-in-parallel.md).
A session that records [reused tasks](#reused-tasks) by reference goes on
packages only with a receiver that also advertises
`X-Putnami-Session-Packages-Rebuilt: 1`: it stores the packages whose header
names rebuilt records, and the reporter's acknowledgements then also say
`packagesRebuilt: true`, which it sends only to an engine of version 5 or
later. Without it, that session stays on frames.
