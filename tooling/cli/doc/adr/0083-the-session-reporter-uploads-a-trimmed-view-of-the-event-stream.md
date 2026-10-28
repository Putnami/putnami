# ADR 0083 — The session reporter uploads a trimmed view of the event stream

- **Status**: accepted
- **Date**: 2026-10-28
- **Amends**: [ADR 0033](0033-native-session-reporting.md), durable delivery
- **Scope**: `@putnami/cli` (`internal/sessionreporter`), feature
  `cli/native-session-reporting`, decisions D-R1, D-R2 and D-R3

## Context

The session reporter sends `events.jsonl` byte for byte. A no-cache run of
about 1,700 tasks writes about 85 MB, and its upload does not finish. On a
recorded 1,610-task session (68.9 MB), `task:event` log lines are 68% of the
bytes, and 99% of them are `debug` lines of tasks that pass. Gzip per chunk
(#669) shrinks the transfer but not what the receiver stores.

The reporter cannot trim. It acknowledges each frame before the engine sends
the next one, and a task's outcome arrives with its `task:end`, often several
frames after its lines. Holding lines in the reporter would acknowledge bytes
no receiver stored, lose them when it restarts, and send other bytes for a
resent sequence, which the receiver refuses.

## Decision

### The engine frames a view

The session reporter's `events.jsonl` frames are cut from a view of the
stream, not from the stream itself. `events.jsonl` on disk stays complete, and
the log reporter keeps the raw stream.

1. The view reads `events.jsonl` record by record. Each decision depends only
   on the records before it, so the view is the same bytes whenever it is
   rebuilt.
2. A record's owner is its `identity.key`. A `task:event` whose
   `event.context` names `batchProjectId` or `batchProjectIds` belongs to each
   `<projectId>:<task name without -batch>`, the member that batch routing
   attributes it to.
3. A `progress` event is dropped.
4. From an owner's first `debug` log line to its `task:end`, every
   `task:event` of that owner is held (D-R1). Other records (`task:start`,
   `task:end`, `test:case`, `session:end`) are never held.
5. At `task:end`, the owner's held records leave in stream order, just before
   it. A `success` or `skipped` end drops the held `debug` lines and keeps the
   others.
6. A record several owners hold is one record and leaves once (D-R3): just
   before the first of their `task:end` records that keeps it, a failure for
   a `debug` line and any end for another line. A `debug` line is dropped when
   every owner that holds it passes. The other owners see it out of order.
7. A `session:end` record releases every held record just before it, so the
   view still ends with it. The final marker releases what is still held. Held
   records leave in stream order.
8. The view holds byte ranges of `events.jsonl`, not records. Past 1,000,000
   held records, a shared record counted once, the owner that holds the most
   releases them at once, as a failure would, and sends live from then on
   (D-R2). A tie goes to the owner whose first held record comes first, then
   to the smaller owner key. A record one of whose owners is live leaves
   live.
9. A line that is not a JSON record leaves as it is, in stream order.

### Frames and checkpoint

- Frames are cut from the view, 64 KiB at most, with view offsets and their
  own sequence. The pending frame in the checkpoint keeps every retry and
  reporter restart byte-identical.
- After an engine restart or `putnami sessions replay`, the view is rebuilt
  from offset 0 and skips to the checkpoint's view cursor.
- The checkpoint moves to version 2. Its events cursor is the view cursor, and
  `eventsRaw` is the raw offset `subscribers.json` acknowledges: every record
  before it is decided, and every byte it put in the view is acknowledged.
  Once the view's final marker is acknowledged, `subscribers.json` reads
  `delivered`.
- A version 1 checkpoint that framed an `events.jsonl` chunk keeps raw framing
  for that session. One that framed none moves to version 2. A CLI that
  predates version 2 refuses the checkpoint instead of resuming raw framing
  at a view offset. The log reporter's checkpoint stays version 1.

## Alternatives rejected

- **Holding only `debug` lines.** A failed task's `info` and `warn` lines
  would leave live, but its debug lines would follow them, and its last 40
  lines would change.
- **No cap on held records.** One owner that never ends would grow memory
  without bound.
- **A shared line that leaves live.** It keeps order, but a batch that passes
  would still send its shared debug lines.
- **Trimming in the reporter.** See the context: acknowledged bytes would not
  be stored, and resent sequences would change.

## Consequences

- The recorded 1,610-task session sends 34.6 MB instead of 68.9 MB.
- A receiver stores what explains the run: every `task:start`, `task:end`,
  `test:case` and `session:end` record, every non-`progress` `task:event` of
  a task that does not pass, and every `info`, `warn` and `error` line.
- A task that logs `debug` lines sends its `info`, `warn` and diagnostic
  events at its end, not live.
- The receiver's offsets describe the view, not the file on disk.
