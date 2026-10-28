# ADR 0001 — A reporter compresses a chunk once the receiver advertises gzip

- **Status**: accepted
- **Scope**: `@putnami/cloud` (`cloud/extension`), the `session-reporter` and
  `log-reporter` processes

## Context

The session reporter posts each 64 KiB frame of a session's files to the
receiver as it is, and the log reporter does the same for the event stream. A
large CI session of about 85 MB is about 1,300 requests of mostly
repetitive JSON text, which gzip shrinks several times.

A receiver that reads gzip decodes the body and stores the decoded bytes. A
receiver that does not read gzip ignores `Content-Encoding` and stores the
body as it arrives, so a compressed body would corrupt its record. Reporters
and receivers deploy independently, so a reporter cannot assume which one
answers.

Most of a large session's bytes are the `debug` lines of tasks that pass. A
task's outcome arrives with its `task:end`, often several frames after its
lines.

## Decision

**Compress only after an advertisement.** A receiver that reads gzip says so
in the `Accept-Encoding` header of every answer (RFC 7694). A reporter sends
a chunk with `Content-Encoding: gzip` only when the latest answer of the same
destination named `gzip` with a nonzero weight. It reads the header on every
answer: the first chunk to a destination, a chunk after an answer without
the header, and a chunk after a request that got no answer go uncompressed.

**The stored bytes do not change.** A chunk with no bytes and a chunk that
gzip does not shrink go uncompressed. The offset and the SHA-256 the log
reporter sends describe the chunk's own bytes. A resent sequence compares
equal whether it is compressed or not.

**Fail closed.** A 200 answer to a compressed body that does not advertise
gzip may come from a receiver that stored the compressed bytes. The reporter
does not acknowledge that frame: it answers `receiver_mismatch` without
retry.

**The reporter does not trim.** It acknowledges each frame before the engine
sends the next one. Holding a line until its task ends would acknowledge bytes
that no receiver stored, lose them when the reporter restarts, and change the
bytes of a resent sequence, which the receiver refuses. Trimming belongs to
the engine, which owns the frames and their checkpoint
([putnami#668](https://github.com/Putnami/putnami/issues/668)).

## Consequences

- A receiver that reads gzip gets smaller requests from its second chunk on;
  the number of requests does not change.
- A receiver that does not advertise gzip gets the same requests as before.
- A receiver rolled back during a run may store one compressed frame. The
  reporter refuses that frame, so the session's delivery fails instead of
  reading delivered over a corrupt record.
