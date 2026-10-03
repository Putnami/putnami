# Session reporter protocol

The engine discovers `session-reporter` on the extension explicitly named by
`PUTNAMI_SESSION_REPORTER`, and `log-reporter` on the extension explicitly named
by `PUTNAMI_LOG_REPORTER` (see [Log reporter](#log-reporter)). Each must resolve
to a single executable command:

```json
{
  "commands": {
    "session-reporter": {
      "visibility": "internal",
      "run": [{ "id": "reporter", "task": "reporter-exec" }]
    }
  },
  "tasks": {
    "reporter-exec": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": ["session-reporter"]
    }
  }
}
```

Declare the runtime normally; preparation uses the existing extension lifecycle
before launch. Stdin carries JSONL chunks and stdout carries JSONL ACKs only.
EOF ends the provider. Core owns its process group and terminates surviving
descendants during teardown. Provider stderr is discarded to avoid leaking
explicit credentials through unstructured diagnostics.

The schema is [`session-reporting.json`](../schemas/session-reporting.json).
Go publishes `SessionReportingChunk`, `SessionReportingAck`, constructors,
parsers and identity matching from `go.putnami.dev/protocol/cli`; TypeScript
exports the twins from `@putnami/cli-protocol`. The TypeScript chunk parser is
asynchronous because it verifies SHA-256 with Web Crypto. Both execute the
[shared conformance corpus](../conformance/session-reporting.json).

A data frame contains `protocolVersion: 1`, `sessionId`, `artifact`
(`events.jsonl` or `session.json`), `offset` in decoded bytes, `sequence` (zero
based per artifact), canonical base64 `data`, lowercase hex `sha256` of decoded
bytes, and `final: false`. Chunks carry at most 65536 bytes; JSONL lines contain
at most 98304 bytes excluding LF. Integers are nonnegative JavaScript-safe
integers. No arbitrary paths, URLs, credentials or destination metadata enter
the protocol.

A final marker is a separate empty chunk with `final: true`, SHA-256 of empty
bytes, EOF offset, and the next sequence ordinal. During execution only
non-final events chunks flow. After graph termination, all session bytes and
the session final marker precede the events final marker. An already pending
events chunk may precede the session, but events final always follows session
final. Chunks may split a JSON line or UTF-8 character: reassemble bytes before
parsing the canonical documents.

ACKs echo `protocolVersion`, `sessionId`, `artifact`, `offset`, `sequence`,
`sha256`, and `final`, plus `ok`. `ok: true` means durable acceptance, including
identical replay, and carries no code or true retryable flag. `ok: false`
requires `code` (`[a-z][a-z0-9_]{0,63}`), optionally `retryable: true`. Human
error messages are not part of the wire. Core verifies every identity field and
never treats a different offset/sequence as progress.

Receivers must accept identical duplicates, reject conflicting content/order,
and bind the destination to the admitted execution. ACK cursors belong to the
original binding. A new credential may renew it, but not retarget it. Core saves
the exact pending frame before sending and saves ACK progress atomically. It
never resets a cursor or rebuilds completed work after delivery failure.

RPC deadlines are five seconds with at most three attempts. Normal finalization
and replay have a thirty-second total budget; already canceled graphs drain for
two seconds. Errors preserve the graph verdict and leave retained state for
`putnami sessions replay --session <id>`. Checkpoint reads are bounded to 192 KiB;
replay validates finalized session documents up to 16 MiB. Retention follows
[`sessions.keep`](../../../tooling/cli/doc/16-session-reporting.md).

`PUTNAMI_SESSION_REPORTER_TOKEN` is an optional explicit provider credential.
On a run without `--credential-fd`, the authoritative CLI captures it before
repository subprocesses, then places it only in the selected reporter's
environment. A hosted run ignores it; see [Run credential](#run-credential).
Selecting a name does not prove the repository manifest/runtime is trusted.
This capability enforces no same-principal, file or metadata/network
isolation; hosted launchers must establish these independently.

## Run credential

A hosted run (`--credential-fd`) hands a reporter the run's credential over
the protocol, never in its environment, arguments or files. The engine removes
`PUTNAMI_SESSION_REPORTER_TOKEN` and `PUTNAMI_LOG_REPORTER_TOKEN` from its own
environment, says so on stderr, and places neither in any reporter's
environment. Version 2 of the protocol is version 1 opened by a two-line
handshake:

1. The engine sends `{"protocolVersion":2,"op":"initialize"}`. It carries no
   credential.
2. A reporter that speaks version 2 answers
   `{"protocolVersion":2,"op":"initialize","ok":true}`.
3. Only then does the engine send
   `{"protocolVersion":2,"op":"authenticate","runCredential":"<credential>"}`.
4. The reporter answers `{"protocolVersion":2,"op":"authenticate","ok":true}`,
   or `"ok":false` with a `code`.

The credential is 1 to 16384 bytes of UTF-8 with no whitespace
(`ValidSessionReportingCredential`). The schema's `maxLength` counts
characters, so only a parser enforces the byte bound. Every answer echoes
`protocolVersion` and `op`; a refusal carries a machine `code`
(`[a-z][a-z0-9_]{0,63}`) and no message. The engine waits at most five seconds
for each answer, and closes a reporter that refuses `authenticate` or does not
answer a line in time; that reporter's delivery fails, and the graph verdict
is unchanged. After the handshake, chunks and ACKs carry `protocolVersion: 1`,
unchanged. An engine without a run credential sends no handshake, so the first
line a reporter reads on a local run is a chunk, byte-identical to before.

**How long it waits.** Each answer has its own five-second limit. The engine
hands the credential to the selected reporters one after another, and each
reporter answers two lines, so the handshake holds the run's first hook for at
most ten seconds per reporter, and twenty seconds with both a session reporter
and a log reporter selected, plus the time to stop a reporter that missed its
limit. A reporter that ignores `initialize` costs five seconds; one that
accepts `initialize` and never answers `authenticate` costs ten.

**A version 1 reporter.** A version 1 reporter does not accept `initialize`,
whose version and members it does not know, and so never reads the
credential. Version 1 does not say what a reporter does with a line it cannot
parse. One that exits fails the handshake at once. One that ignores the line
and keeps reading is closed when the five-second limit for `initialize`
passes, so it delays the run's first hook by five seconds. A hosted run starts
a reporter that does not accept `initialize` again without a credential: no
handshake and no token. The engine prints that the reporter cannot hold the
run credential and starts without one. The reporter's own destination then
decides whether it delivers.

**Who holds it.** A hosted run hands the credential only to a reporter that
runs as its extension's native runtime executable (`{extensionRuntime}`), and
only before the first repository code runs. Any other reporter command starts
without a credential, with the same diagnostic. The run starts every selected
reporter before its first hook, install or job, and the session's delivery
reuses that process. A reporter that a hosted run must start again after
repository code ran, after a crash for example, gets no credential: its
delivery fails. What it did not deliver stays pending in the session's
checkpoint, and `subscribers.json` records it. The hosted run does not replay
it; a later `putnami sessions replay --session <id>` is a separate invocation
and needs a reporter credential of its own. A hosted run refuses `--watch`,
and `serve`, which always watches, because every watch iteration starts after
repository code ran. A reporter keeps the credential in memory only, and
should deny inspection of its process
(`go.putnami.dev/sdk/extension/procguard.DenyInspection`) before it answers
`initialize`.

Go publishes `SessionReportingHandshake` and `SessionReportingHandshakeResult`,
their constructors and parsers, and `SessionReportingCredentialVersion`;
TypeScript exports `parseSessionReportingHandshake`,
`parseSessionReportingHandshakeResult` and
`sessionReportingHandshakeResultAnswers`. The shared corpus covers both lines.

## Log reporter

`log-reporter` is a second reporting capability for an extension that needs
only the live event stream. It uses this wire unchanged, restricted to
`events.jsonl`: every frame and ACK names `events.jsonl`, and the provider never
receives `session.json`. Its events final marker follows graph termination.

| | `session-reporter` | `log-reporter` |
| --- | --- | --- |
| Selector | `PUTNAMI_SESSION_REPORTER` | `PUTNAMI_LOG_REPORTER` |
| Optional token | `PUTNAMI_SESSION_REPORTER_TOKEN` | `PUTNAMI_LOG_REPORTER_TOKEN` |
| Artifacts | `session.json`, then `events.jsonl` | `events.jsonl` |
| Live chunk leaves at | a full frame or 10 s | a full frame or 2 s |

Each selected capability is a separate subscriber of the session event stream,
with its own provider process, cursor and
[`subscribers.json`](05-session-subscribers.md) entry. Selecting one never
selects the other. One extension may serve both by declaring both commands.
Each token reaches only its own provider, never a task, a hook or the other
provider. On a hosted run, both receive the run credential over the
handshake instead. One capability's outage, refusal or crash changes neither the other
capability's delivery nor the graph verdict.

Go publishes `LogReporterCommand`, `LogReporterEnv`, `LogReporterTokenEnv` and
`SessionReportingArtifacts(command)`; TypeScript exports `LOG_REPORTER_COMMAND`,
`LOG_REPORTER_ENV`, `LOG_REPORTER_TOKEN_ENV` and `sessionReportingArtifacts`.
A corpus case may name its `reporter`; both runtimes reject a case whose
artifact that reporter never transmits.
