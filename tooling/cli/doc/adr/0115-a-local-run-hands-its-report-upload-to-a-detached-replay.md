# ADR 0115 — A local run hands its report upload to a detached replay

- **Status**: accepted
- **Date**: 2026-11-27
- **Amends**: [ADR 0033](0033-native-session-reporting.md), durable delivery
- **Amended by**: [ADR 0131](0131-a-session-reporter-sends-larger-frames-once-its-receiver-advertises-them.md):
  larger frames, rejected here, are negotiated with the receiver
- **Scope**: `@putnami/cli` (`internal/sessionreporter`, `internal/engine`,
  `internal/cli`), feature `cli/native-session-reporting`

## Context

A run that selects the session reporter drains its delivery before it
returns (ADR 0033): once the graph ends, the worker sends what remains of
`events.jsonl`, then `session.json` and `spec-verification.json`, one frame at
a time, each acknowledged before the next.

Measured on 2026-11-27, on a 4-core Linux container, on an unchanged
`lint,test,build,validate --all` of this repository, signed in to Putnami
Cloud (the session reporter in local mode), with a CLI that timed each frame:

| | Wall |
|---|---|
| Reporter selected | 100.6 s, 100.7 s |
| Reporter not selected | 29.1 s, 31.2 s |

The drain took 70.3 s after the summary. It sent 402 frames (events 314,
session 40, spec verification 48), 26.0 MB, strictly one after another. Each
frame's round trip took 125 ms at least, 140 ms at the median, 203 ms at the
90th percentile; their sum is 68.5 s. There was no retry and no refused
frame. The cost is the count of frames times the latency of one acknowledged
request, not bandwidth.

Two other answers were measured against that profile and rejected:

- Larger frames, or several frames in flight, change the wire and the
  receiver's sequence contract (one-way doors). At 140 ms per acknowledged
  request, even a fourfold cut of the request count leaves about 17 s.
- Trimming the payload (ADR 0083's view) is a separate question: an unchanged
  run's view is the whole stream, mostly `test:case` records of cached tasks
  (16.6 MB of 26.5 MB). It reduces the drain, never removes it.

## Decision

1. A local run returns once its result is durable locally. At the end of a
   terminal run, the engine stops each reporting worker instead of draining
   it. A stopped worker keeps what ADR 0033 already makes durable: its
   checkpoint, with the exact pending frame, and its acknowledged cursors;
   the session directory is the spool. Its evidence records the partial
   delivery.
2. The engine then starts, outside every process tree of the run,
   `putnami sessions replay --session <id>` for the run's session, with the
   captured selectors and tokens. The replay is the existing one: it resumes
   each capability from its own checkpoint, under the finalization limits of
   a run, and rewrites the evidence once it delivered.
3. The same run also starts a detached replay for each of the three newest
   other retained sessions that a replay owes and no process delivers: the
   session is finalized, the checkpoint of a selected capability is
   incomplete, and that capability's lock is free. It prints one line naming
   how many and the newest of them. What an interrupted replay, an offline
   machine, a sleep or a logout left is delivered by a later run. A session
   killed before it was finalized is never owed: no replay accepts it, as
   ADR 0033 already holds for executor loss.
4. Exactly once is the checkpoint's: a frame leaves only after it was saved
   as pending, an acknowledgement moves the cursor only once the checkpoint
   is durable, and a resent pending frame has the same identity and bytes,
   which the receiver accepts as an identical duplicate. A stop that cancels
   a frame's last attempt keeps it pending, instead of omitting plan.json as
   undeliverable. A replay holds the capability's lock; a second replay of
   the same session fails at once and sends nothing.
5. The synchronous drain stays where the process must deliver before it
   exits: a hosted run (a run credential, ADR 0055), a CI job, whose machine
   may be reclaimed at exit, a run whose session could not be finalized, and
   every adapter but the terminal job commands (a `cache verify` audit, an
   extension alias, a workspace lifecycle job, a bound request). A CI job is
   one with `CI` set, which most CI systems set, or with `JENKINS_URL`,
   `TF_BUILD` or `TEAMCITY_VERSION`. Setting `CI` is also how any other
   environment that must deliver before it exits, such as a container
   reclaimed when its command ends, keeps the drain.
6. The detached replay receives the captured selectors and tokens in its
   environment, as the pinned CLI does on relaunch. This adds no reader: the
   run's own process environment holds the same tokens from its exec on
   (capture removes them from the running process only), and each provider
   receives its token in its environment (ADR 0033); every one is readable
   by the same user only. A signed-in local run of the cloud reporter
   carries no token at all: it reports under the stored login.

## Consequences

- A signed-in unchanged run returns with the signed-out wall; its upload runs
  after it, as a background process the user does not wait for. Measured
  after the change on the same machine, four pairs: 32.4, 37.4, 30.9 and
  32.4 s signed in against 30.7, 32.2, 31.2 and 32.4 s without the reporter,
  a median gap of 0.7 s. The detached replay delivered each session about
  75 s after its run returned.
- The replay prepares its provider and loads the workspace again, a cost the
  machine pays in the background. A session whose delivery keeps failing is
  named and retried by each later run, while it is among the three newest
  owed sessions, until session retention expires it: pending sessions lead
  `sessions.keep`, which bounds the spool.
- The detached replay's output is discarded. Its outcome stays in the
  session: the checkpoint (its pending frame and error) and the
  `subscribers.json` evidence that `putnami sessions inspect` prints. A
  replay writes only there, under `.putnami/sessions/`, never elsewhere in the
  worktree. `putnami sessions replay` still delivers by hand.
- A replay that a logout or a closed terminal kills, or that a sleeping
  machine stalls past its limits, leaves its checkpoint; a later run names
  and delivers it.
- Windows builds this code: `proctree.StartDetached` starts the replay
  there. It is not run or measured on Windows.
- A later machine-level process may own the upload; nothing here depends on
  it.
- What an unchanged run uploads is left as it is: its view is the whole
  stream, mostly `test:case` records of cached tasks. Cutting it changes
  what the receiver sees, which this decision does not.
