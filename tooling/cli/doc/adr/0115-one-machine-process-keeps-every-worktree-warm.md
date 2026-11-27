# ADR 0115 — One machine process keeps every worktree warm

- **Status**: proposed
- **Date**: 2026-11-27
- **Amends**: `protocols/workspace`
  [ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md),
  its rejected alternative "A background program that watches the tree"
  (section 10)
- **Depends on**: ADR 0113, "An input digest whose witness holds is not
  collected again" (proposed), whose section 7 is the interface this process
  answers to; ADR 0112 (the engine's own Git reader)
- **Scope**: `@putnami/cli` (a new `internal/machine` package: the process,
  its socket, its watcher and what it holds; `internal/cli`: the client;
  `internal/engine` and the packages it calls: the run's process context
  taken as a value); `putnami machine` commands; the global configuration

## Context

An unchanged run loads the workspace, reads its configuration, synchronizes
the extension runtimes, checks the workspace snapshot, plans, computes or
checks every key, and records the session, from nothing, in a new process.
Bazel, Gradle, Pants and Nx keep a process for this reason. The work that
made unchanged runs cheap in November 2026 refused a
background program (ADR 0009 of `protocols/workspace`, "Rejected
alternatives") because one per worktree does not fit a repository worked
through many worktrees. The owner reconsidered on 2026-11-26 and confirmed on
2026-11-26 and 27: one process per machine and user, shared by every
worktree, is in scope, and distributed execution stays a design in
this epic.

Measured on 2026-11-27 at `299343dc` (main), on the 4-vCPU Linux container
of the earlier measurements (ext4 root, 15 GiB), shallow clone, signed out of
Putnami Cloud: `./putnamiw lint,test,build,validate --all --enforce-coverage`
run once cold to prime the caches (1,085 s), then three times on the
unchanged tree. Wall is the `./putnamiw` call; engine CPU is
`run.orchestration.cpu.engineMs`; peak memory is the engine process's `VmHWM`
sampled every 0.3 s; the live heap is read from `GODEBUG=gctrace=1` on a
fourth run.

| Measure, unchanged run | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| Wall | 25.6 s | 23.1 s | 22.8 s | 23.1 s |
| Engine CPU | 31.9 s | 35.5 s | 33.6 s | 33.6 s |
| Key time summed over tasks | 11.6 s | 16.6 s | 14.4 s | 14.4 s |
| Restore time summed over tasks | 7.9 s | 8.3 s | 7.7 s | 7.9 s |
| Longest ready wait | 9.6 s | 9.3 s | 8.6 s | 9.3 s |
| Engine Git calls | 17 | 17 | 17 | 17 |
| Job processes | 60 | 51 | 51 | 51 |
| Engine peak memory (`VmHWM`) | 663 MiB | 801 MiB | 776 MiB | 776 MiB |

Of the 2,245 selected tasks, all but the job processes are served from the
local cache. The phases before execution take 2.8 s of wall: workspace-load
61 to 67 ms, runtime-sync 208 to 303 ms, probe 759 to 814 ms, selection 2 to
3 ms, plan 1,604 to 1,756 ms. The 17 Git calls are `rev-parse` 9, `ls-files`
2, `status` 2, and one each of `check-ignore`, `config`, `show` and
`symbolic-ref`; a full clone with tags makes 508 since the engine reads Git itself (ADR 0112), of which the
482 release-baseline reads go to ADR 0113 §4.

The engine's live heap, the heap a collection keeps, is 35 MB once the
workspace is loaded, 44 to 51 MB once the run is planned, and grows to 209 to
234 MB at the end of the run, with the run's results and events; at the
200 percent target of ADR 0111 the heap goal reaches 702 MB. Each run writes
a session of 34 MB, of which `events.jsonl` is 27 MB.

Starting the CLI costs 17 to 23 ms for the binary (`putnami --version`) and
151 to 169 ms through `./putnamiw`, which computes the content key of the
CLI's sources first.

A second worktree of the same repository (`git worktree add --detach`, same
commit, shared store): its first run takes 54.2 s, of which about 30 s is
the implicit install of the workspace's ten extensions before the session
starts (`controlMs` 8.4 s) and a probe of 2.9 s; its second and third runs
take 24.7 s and 23.4 s.

A small change, measured in that second worktree: a new comment line
appended to `go/framework/client/binary_call.go` before each run, three runs.
The first ran signed out: 118.9 s wall. The CLI was signed in during the
second, so the second and third include the session upload (153.8 s and
182.8 s wall, of which the run reports 1.3 and 1.7 minutes). Across the three:
621 job processes, engine CPU 44.9 to 48.6 s, extension CPU 33.1 to 35.7 s,
19 to 20 engine Git calls, and before execution runtime-sync 3.3 to 3.6 s
(the change moves a local extension's runtime), probe 2.1 to 2.3 s and plan
2.1 to 2.2 s.

A throwaway watcher (`inotify`, one watch per directory the default walk
enters, `IN_CREATE|IN_DELETE|IN_MOVED_FROM|IN_MOVED_TO|IN_MODIFY|IN_ATTRIB|
IN_CLOSE_WRITE|IN_DELETE_SELF|IN_MOVE_SELF`) watches this worktree's 2,955
directories in 27 to 31 ms, and a copy of its 2,532 tracked directories in 14
to 38 ms, with under 2 MB of the watcher's own heap. Its first event arrives
under a millisecond after the write. A reader that keeps draining the queue
received all 60,000 events of 30,000 files created in 384 ms with no
overflow (the container's `max_queued_events` is 16,384, `max_user_watches`
128,412, `max_user_instances` 128).

What this leaves, by what holds it today:

- what ADR 0113 makes cheap but not free: a stat per witnessed path (28 ms for
  the whole tree), the record's load (4 ms), and the key's other inputs;
- what no record removes: process start, the workspace load, runtime-sync,
  the snapshot check, the plan (2.8 s together), the Git reads, and the
  session's 2,245 task results;
- what other work removes: the job processes of tasks that are never cached
  (the uncached-task work: such a task says why it cannot be cached), and
  the per-task fixed costs of a hit, restore, sanitizing, negotiation and GC
  (the per-task fixed-cost work). Signed in, the session upload adds 55 to
  70 s after the run (the session-upload work).

## Decision

### 1. One process per machine, user and CLI build

A machine process serves every worktree of every workspace of one user on one
machine. It is the CLI's own executable, started as `putnami __machine
serve`, so it runs the code of the build that started it.

**Its identity is the CLI's program identity.** The identity is the one the
file index already reads (`fileindex.ExecutableIdentity`: the reading-scheme
version, the build information and the Go build ID, ADR 0009 §4 of
`protocols/workspace`). The process's socket and lock are named by the first
16 hex digits of its SHA-256. A CLI of another build therefore looks for
another socket and never reaches this process; the handshake (section 2)
compares the full identity again. A CLI whose identity is empty (no usable
build ID) never starts or uses a process.

**At most two processes per user.** In this repository each worktree at a
different commit builds its own CLI (`putnamiw` keys the binary by its
sources), so two builds can be in use at once. A client that starts a process
when two others of the same user run asks the least recently used one to stop
(section 3). A workspace that pins one released CLI sees one process.

**Discovery.** The process's directory is, in order:

1. `$XDG_RUNTIME_DIR/putnami` on Linux when the variable names an existing
   directory owned by the user with mode `0700` (a per-user tmpfs that the
   session manager empties at logout);
2. `$TMPDIR/putnami-<uid>` on macOS (`$TMPDIR` is the user's private
   `/var/folders/…/T/`);
3. `~/.putnami/machine` otherwise.

The directory is created with mode `0700` and used only when the user owns it
and no other user can write it, checked with `lstat` (a symbolic link is
refused). In it, `<id>.sock` is the socket, `<id>.lock` the liveness lock and
`<id>.json` the process's description (identity, process id, start time,
version, the worktrees it holds), written after it listens. A socket path
longer than the platform's limit (108 bytes on Linux, 104 on macOS, the limit
`hostrpc` already handles) falls to the next directory; none fits, no process.

**Start.** A run that finds no process runs on its own, as today, and starts
one when it ends, detached in its own session (`proctree.StartDetached`,
`setsid`), with stdin, stdout and stderr on `/dev/null` and its log at
`<id>.log` (rotated at 4 MiB). The start never delays the run that asks for
it: the run has exited when the process loads anything. Two starters
serialize on `<id>.lock`: the process holds it exclusively for its whole life
(`flock`), so a starter that cannot take it knows a process lives, and one
that can removes a stale socket before starting. The process loads the
worktree that started it in the background, then waits for requests.

**Stop.** The process stops:

- **idle**: after 3 hours with no request (Bazel's and Gradle's default);
- **version change**: when its executable's path no longer names the file it
  started from (checked by stat on each request and every minute), since a
  rebuilt or removed CLI cannot start the same code again for its nested
  calls;
- **request**: `putnami machine stop`, or a newer client evicting the least
  recently used of two (section 3);
- **memory**: when its held state cannot be brought under the ceiling of
  section 7;
- **crash**: a panic or a kill ends it like any process. The lock is released
  by the kernel, so the next run finds no process and runs on its own. A
  process that ended without stopping records it in `<id>.json`; after three
  such ends within an hour no run starts it again for that hour, and
  `putnami machine status` says why.

A stopping process finishes the runs it serves, refuses new ones (their
clients run on their own), removes its socket and description, and exits.

### 2. The socket and the run it serves

**The machine process runs the engine; the client is its front end.** A
client that finds a process of its identity connects to `<id>.sock` (a Unix
stream socket, mode `0600`), sends a handshake (protocol version, full
program identity) and a run request: its arguments, working directory,
environment, umask, the terminal's size and modes, and its standard input,
output and error as file descriptors (`SCM_RIGHTS`). The process runs the
engine for that request with exactly that context, writes to those
descriptors, receives the signals the client forwards (interrupt, terminate,
window size), and answers the exit code, which the client exits with. A
client that loses the connection mid-run reports it and exits with the code a
lost run has today (an interrupted run); the process cancels a run whose
client disconnected, as an interrupt does.

The engine therefore takes its process context as a value: environment,
working directory, standard streams, home directory, clock and signal source.
No package the engine reaches reads `os.Environ`, `os.Getenv`, `os.Getwd`,
`os.Stdout` or the process's signals; the entry point reads them once and
passes them in. A test fails on such a read outside the entry point (a static
check over the engine's import graph, 123 reads in 64 files today). Global
memos that outlive a run today (the workspace load memo, the file index in
use, the toolchain probe answers) become fields of the worktree's held state
or of the run.

**Which runs go through it.** Every run the process can serve the same way,
and none other. A client runs on its own when:

- the process is turned off (section 9), or the client is in CI (`CI` set),
  or it is a hosted run (`--credential-fd`, ADR 0055), or a nested run (a
  `putnami` started by a job of a run, which carries
  `PUTNAMI_MACHINE_RUN`): a nested run through the process could wait on the
  run that started it;
- the command is not a run of the engine (`config`, `machine`, `cache gc`,
  `install`, `--version`…), or it is `watch` or `serve`, which already keep
  their own process;
- a run of the same worktree is in progress in the process (the second run
  runs on its own, with today's locks, as two runs do today);
- the process refuses it: stopping, over its memory ceiling, or another
  identity.

A refused or failed connection costs one `connect` and the run continues on
its own: the process never stands between the user and a run.

**What a run inside it does.** The same code as a run on its own, with held
values in place of what the run would compute, each under the witness rule of
section 5. Its jobs are children of the machine process, started with the
client's environment, in the client's working directory and process group
policy, as the engine starts them today. Its session is the same session,
written in the same place, with one more member, `run.machine` (whether the
run went through the process, the held values it used, and the reason when
it did not).

### 3. What it holds

Held state lives in memory only. The process writes nothing a run on its own
does not write; what a run writes to disk through it (the file index, the
input record of ADR 0113, the workspace snapshot, the session) it writes as a
run on its own does, so a run without the process finds the same files.

**Per worktree**, keyed by the worktree's root and the Git common directory:

| Held value | What it replaces | Witness |
|---|---|---|
| The loaded workspace: merged configuration, extension discovery and placement, the probe view | workspace-load, runtime-sync's discovery, probe (0.8 to 1.1 s) | The workspace snapshot's inputs (ADR 0009 §7 of `protocols/workspace`) |
| The file index of ADR 0009 | its load and save | Each file's stat |
| The input record and release baselines of ADR 0113 | the record's load, and each witnessed stat | ADR 0113 §1 to §4 |
| The Git reader's state of ADR 0112: parsed index, HEAD, refs, configuration, the cut | the engine's Git calls (17, or 26 on a full clone after ADR 0113) | The index stamp, the files of `.git` it read, the environment it read |
| The last plan per request shape (command, selection, options, parameters, the environment's digest) | plan (1.6 to 1.8 s) | Every value the plan read: the loaded workspace, the configuration, the request |
| Each task's last key, its inputs' identities, and the outputs it left on disk | composing a key whose inputs are the same values; checking outputs that were not touched since | The inputs' own witnesses; the outputs' paths, watched since the run that wrote or checked them |

A held plan or key is a memo of a function the run computes anyway: it is
used only when every value it was computed from is the same held value, or
equal to it. A request whose environment digest differs plans again.

**Shared by every worktree:**

- **The store.** Nothing of it is held but its root and the open lock: a
  cache entry is read from disk. The process runs the store's opportunistic
  GC (`store.MaybeOpportunisticGC`, [Caching](../10-caching.md)) when idle, so a run no
  longer pays it at its end.
- **Extension runtimes.** The runtime-sync answers (the runtime-info
  handshake of ADR 0102), held under the witness of the runtime's files.
- **Toolchain probe answers** of ADR 0102 and ADR 0105, held under the stat of
  the probed binary (today they are cleared at every run start).
- **The extension worker pool: not in this record.** Every job is a process
  of its own (one per task, one per batch), and an extension serves no second
  job. A pool needs an extension protocol in which an extension process
  serves several jobs, which `protocols/workspace` ADR 0001 rejected for
  providers. On an unchanged run, extension CPU is 0.8 to 1.3 s for 51 to 60
  job processes, which the uncached-task work removes. A pool would gain on
  the small change
  (33.1 to 35.7 s of extension CPU for 621 job processes) and on a cold run,
  where sdd and go spend 56 s and 42 s, CPU that separate work on those
  extensions reduces first. The pool is its own record, sized on what that
  work and the uncached-task work leave.
- **Warm toolchains**: the toolchains stay what they are, files under the
  Putnami home, and the process keeps their probe answers. It starts no
  language server or build daemon (`gopls`, `tsserver`, the Gradle daemon):
  a tool's own daemon is the tool's choice, started by its job.

**Session upload, optional.** A signed-in run may hand its finished
session to the machine process for upload instead of waiting for it (about
60 s on an unchanged run). The process holds the reporter's token for that
upload only, in memory, and drops it once the upload ends or fails. Without
the process, the session-upload work's own design applies; this record
fixes only that the
process may own the upload, never that a run needs it.

### 4. How it learns of changes

**One watcher per process**, one watch set per worktree. On Linux, one
`inotify` instance (the user has 128), one watch per directory: the
directories the default walk enters (the skip list of ADR 0113 §1), the
directories of every witnessed path, `.git` and its `refs` directories, the
workspace's configuration files' directories, and the toolchain and
extension-runtime directories it holds answers for. On macOS, one FSEvents
stream per worktree root with file-level events
(`kFSEventStreamCreateFlagFileEvents`), and one for each held directory
outside it.

**A sync point before each run.** A run asks the watcher for a sync point:
the process writes a cookie file in the worktree's `.putnami/` (watched, not
walked) and waits for its own event. Every write that completed before the
cookie was written has been delivered by then (Watchman's method; on macOS
`FSEventStreamFlushSync` first). A sync point not reached in 2 s is a break.

**A path holds without a stat only as ADR 0113 §7 says**: the process has
watched it, or the directory that holds it, without a break since it last
took its stat, on a file system ADR 0113 §2 trusts. Every one of these is a
break, and a break makes the run take the stat:

- a queue overflow (`IN_Q_OVERFLOW`; on macOS `MustScanSubDirs`,
  `UserDropped`, `KernelDropped`): a break for every path of the watch set;
- a watch removed (`IN_IGNORED`, an unmount), a watched directory renamed or
  removed (`IN_MOVE_SELF`, `IN_DELETE_SELF`): a break below it;
- a directory created: watched from the event on, and every path below it
  is taken by stat until a stat after its watch was added;
- a watch the kernel refused (`ENOSPC` at `max_user_watches`): that
  directory and everything below it is unwatched and taken by stat;
- a sync point not reached, a watcher restarted, or a process just started:
  every path is taken by stat once;
- a file system ADR 0113 §2 does not trust (NFS, SMB, 9p, FUSE, FAT), and
  Windows: never watched, no record anyway.

**The limits.** `max_user_watches` is shared with every other watcher of the
user (editors, other tools): the process uses at most half of what is left
when it starts, read from `/proc/sys/fs/inotify/max_user_watches` and its own
count, and a worktree that does not fit is watched for its most recently
used directories, the rest taken by stat. This repository needs about 3,000
watches; the kernel charges each against the user's memory (about 1 KiB
worst case on 64-bit). macOS coalesces FSEvents and may report a directory
instead of a file: an event for a directory is a break for everything below
it, which costs stats, never a missed change. `mmap` writes raise no event
until the file is closed or its times move, the risk ADR 0009 already names
for a stat; overlayfs reports writes made through the overlay, and a write to
a lower layer under a mounted overlay is unsupported by the kernel.

**A sampled self-check.** At each run the process also takes the stat of 64
paths it would have answered without one, chosen at random; a stat that
differs from what the watch says is a break for every worktree, logged and
counted in `putnami machine status`. A watcher bug shows as stats, not as a
stale key.

### 5. It accelerates and never decides

Every held value is answered under a witness, the witness is the one a run on
its own checks, and the process may skip a check only by ADR 0113 §7's rule.
A held value whose witness the process cannot vouch for is computed again, as
a run on its own computes it. Hence:

- a run on its own, in CI or on a cold machine, computes the same keys, plans
  the same tasks, and gets the same results and the same exit code;
- no key, entry address, record format or protocol value changes, and nothing
  the process holds is written as a value another run trusts.

**A comparison test checks it.** A test of `internal/machine` starts a
process on a Git fixture, runs the same commands through it and on their own
(`PUTNAMI_MACHINE=off`) after each round of the randomized writes of ADR 0113
§5 (and a branch switch, a stage, a configuration edit, an extension runtime
change), and compares the plan, every task's key, every task's outcome and
the exit code. A difference fails the test. The same test runs with a
watcher that drops events at random and reports the drops as overflows, and
with one that reports none of a round's events while the stat check of
section 4 is forced on every path: both must give the same results. The
orchestration counts tests (`internal/cli/e2e/orchestration`) run both ways.

CI never starts a process (section 2), so no CI result depends on one.

### 6. Versioning

A CLI never talks to a process of another identity: the socket name differs,
and the handshake refuses a mismatch with `machine.identity`, after which the
client runs on its own. The process's held state never leaves it, so no
format crosses builds. The handshake carries a protocol version; a process
answers a client of the same identity only, so the version moves with the
build and exists for the error message.

### 7. Memory

**Ceiling: 1 GiB per process, or one eighth of the machine's memory if that is
less**, set as the Go runtime's soft limit (`debug.SetMemoryLimit`) in the
process, with the 200 percent target of ADR 0111 below it. Its parts:

- **Held state, at most 256 MiB.** Planned, this repository's engine holds 44
  to 51 MB of live heap; the record of ADR 0113 is about 2.8 MB on disk. A
  worktree's held state is measured by the prototype (section 11) and
  accounted per worktree; past 256 MiB the least recently used worktree is
  dropped, and a dropped worktree costs its next run a run on its own.
- **A run.** A run inside the process peaks where a run on its own does:
  209 to 234 MB of live heap, 663 to 801 MiB of resident memory at the
  200 percent target. A run is admitted only when the held state plus the
  measured peak of that worktree's last run fits under the ceiling;
  otherwise its client runs on its own. Two runs of two worktrees can run
  together when both fit.
- **Between runs** the process returns the run's heap to the system
  (`debug.FreeOSMemory` after the run's results are written), so an idle
  process keeps about its held state.

A process that stays above its ceiling for a minute with no run drops its
worktrees; one still above it stops (section 1).

### 8. Security

- The socket lives in the directory of section 1, owned by the user, mode
  `0700`, and is itself `0600`. No TCP port, no loopback HTTP.
- The process accepts a connection only from its own user: it reads the
  peer's credentials (`SO_PEERCRED` on Linux, `getpeereid` on macOS) and
  closes any connection whose user id differs from its own. Root talks only to
  root's process, and a user's CLI never to root's.
- Same-user is the boundary, as it is for the store and the file index today:
  a process of the same user can already read the user's files and trace the
  user's processes.
- A run's environment, credentials included, is held for that run only and
  dropped when it ends; nothing of it is logged. Held state keeps environment
  digests, never values. A hosted run, whose credentials ADR 0055 keeps out of
  repository processes, never goes through the process.
- A job of a run inside the process carries `PUTNAMI_MACHINE_RUN`, and a CLI
  that sees it runs on its own: a job reaches no other worktree through the
  process.

### 9. Settings and commands

- **Setting**: `machine.process` in the global configuration
  (`~/.putnami/config.json`): `on`, `off`, or absent for the platform's
  default; `PUTNAMI_MACHINE=off|on` overrides it for one command. A
  workspace's configuration may say `off` for its own runs, never `on`.
- **Default**: on on Linux; off on macOS until the measurement protocol of
  section 11 has run on a Mac and the owner has turned it on; Windows has no
  process (decision `windows-support-is-suspended`).
- **Commands**: `putnami machine status` (the processes of the user, their
  identities, worktrees, held memory, watches, breaks, self-check results and
  last stop reason), `putnami machine stop [--all]`.

### 10. The refusal of a background program is changed

`protocols/workspace` ADR 0009 rejected "a background program that watches the
tree" because one per worktree does not fit many worktrees and it gives
nothing to CI or to a cloud session. This record keeps both facts and changes
the conclusion: one process per machine and user serves every worktree, and a
run without it, in CI or in a cloud session, is a run as today. ADR 0009's
rejected alternative is amended to point here, and the decision
`a-machine-process-serves-every-worktree` records it. The other rejections of
a daemon stand: the registry credential is still not served by a socket
(ADR 0101, registry ADR 0001), and an extension provider is still not a
daemon (`protocols/workspace` ADR 0001); a pool of extension workers would
change the latter in its own record (section 3).

### 11. Prototype plan and measurement protocol

Before the implementation, a prototype on a branch of its own, measured and
then thrown away or folded into the first slice:

- `putnami __machine serve` with one worktree: socket, handshake, peer
  check, the client's descriptors and environment, the engine run in the
  process; held: the loaded workspace, the file index, the input record (once
  ADR 0113 lands; before, the in-run memo of ADR 0109 kept across runs under
  the watch), the Git reader's state, the plan;
- the Linux watcher of section 4 with its sync point and break rules;
- the comparison test of section 5 on the fixture, and on this repository's
  unchanged run (the keys and outcomes of the three runs on their own and
  through the process compared one by one).

**Protocol.** This container class: 4 vCPU Linux, ext4, signed out, the
current commit of main and the prototype's branch, on a shallow clone and on
a full clone with tags. Each scenario runs once to prime, then three times
with the process warm and three times with `PUTNAMI_MACHINE=off`, medians
reported:

1. **Unchanged run**: `./putnamiw lint,test,build,validate --all
   --enforce-coverage` on the unchanged tree.
2. **Small change**: the same command after appending a new comment line to
   `go/framework/client/binary_call.go` before each run.
3. **Second worktree**: `git worktree add --detach` at the same commit, then
   the same command in it, first run and second run, with the process
   already holding the first worktree.

Measured each time: wall of the `./putnamiw` call and of the binary called
directly; engine CPU (on its own, `engineMs`; through the process, the
process's CPU over the run, one run at a time); engine Git calls; job
processes; key, restore and ready-wait times; resident peak of the client and
of the process; the process's held memory per worktree after the run; the
watch count and setup time; the breaks and self-check results.

**Expected, and what the prototype must show.** Through the process the 2.8 s
before execution and the 17 Git calls go to about nothing. The issue's target,
under 2 s for an unchanged run, also needs the uncached-task work (no job process on an
unchanged run) and the per-task fixed-cost work (no per-task fixed cost on a
hit whose outputs are on disk): with them, each of 2,245 hits costs the held key's check and its
session record, which must stay under 0.5 ms of CPU each (1.1 s for 2,245 on
four workers), and the 34 MB session must be written in a fraction of a
second. The prototype reports each of these parts; a target it cannot reach
is reported with the part that holds it.

## Consequences

- An unchanged run through the process makes no Git call, starts no process
  of its own before its first job, and plans nothing it planned before; a
  second worktree starts with the shared state (runtimes, toolchain answers,
  store) warm, and its own state cold.
- The engine takes its process context as a value: a refactor of its 64 files
  that read the environment, and a static check that keeps it so.
- A process can outlive a CLI build by up to 3 hours, using memory up to its
  ceiling; `putnami machine status` and `stop` show and end it.
- `inotify` watches count against the user's limit, shared with editors.
- A run through the process starts its jobs from the process, not the
  client's shell: they see the client's environment and descriptors, but
  their parent is the process. A tool that reads its parent's identity sees
  `putnami __machine serve`.
- macOS waits for its measurement; Windows has none.

## Rejected alternatives

- **One process per worktree.** ADR 0009's reason holds: this repository is
  worked through many worktrees (measured: a second worktree's first run is
  54.2 s, of which the shared state is a part), and per-worktree processes
  would each hold the shared state and the 44 to 51 MB of a loaded workspace.
- **Strictly one process per user, whatever the build.** Two worktrees at two
  commits of this repository run two CLI builds; a single process would be
  stopped and started at each alternation, and its held state lost each time.
- **A state service that the run asks, the engine staying in the client.**
  The client would still start, load the workspace, synchronize, probe and
  plan: 2.8 s of the measured run before execution, over the 2 s target on
  its own, and a plan cannot cross a process boundary without being encoded
  and decoded each run. Nx and Turborepo keep such daemons (the project
  graph, file hashes); their unchanged runs at this repository's scale are
  not measured yet; the scale bench of this epic measures them.
- **The watcher alone decides, with no witness.** Dropped events, a cold
  machine and CI would serve stale keys; ADR 0113 §7 and the epic's rule keep
  the witness the decider.
- **A recursive or file-system-wide watch.** `fanotify` with a mount or file
  system mark needs `CAP_SYS_ADMIN`; unprivileged `fanotify` (Linux 5.13)
  takes inode marks only, as `inotify` does.
- **Polling, as `internal/watch` does for `watch` mode.** A poll of this
  tree is the stat of every witnessed path (28 ms, ADR 0113), which a run
  without the process already pays once; a poll every 500 ms costs it 7,200
  times an hour for an idle process.
- **A pool of extension workers now.** It changes the extension protocol for
  0.8 to 1.3 s of extension CPU on an unchanged run, which the uncached-task work removes. Its
  gain is on small-change runs (33.1 to 35.7 s of extension CPU, measured
  above) and cold runs, whose extension CPU the work on sdd and go reduces
  first; the pool's record is sized on what that work leaves.
- **The process persists its held state to disk.** That is ADR 0113's record,
  which a run on its own already writes; a second on-disk form would be a
  second thing to keep correct.
