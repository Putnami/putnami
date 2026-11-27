# ADR 0116 — One machine process keeps every worktree warm

- **Status**: proposed
- **Date**: 2026-11-27
- **Amends**: `protocols/workspace`
  [ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md),
  its rejected alternative "A background program that watches the tree"
  (section 13)
- **Depends on**: ADR 0113, "An input digest whose witness holds is not
  collected again" (proposed), whose section 7 is the interface this process
  answers to; ADR 0112 (the engine's own Git reader); ADR 0115 (the detached
  report upload, which the supervisor may later own)
- **Scope**: `@putnami/cli` (a new `internal/machineproc` package: the supervisor,
  its socket, its watcher, its admission and its protocols, and the engine
  worker and what it holds; `internal/cli`: the client;
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
worktree, is in scope, and distributed execution stays a design in this
epic. On 2026-11-27 the owner set its shape: one supervisor per machine and
user that owns the machine's execution flow, and behind it one engine worker
per CLI build in use, with no fixed cap, compared with one process per
worktree. Its default, first set to on for Linux and macOS, is decided on
measurements (section 12).

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
CLI's sources first. A CLI process that loads nothing (`--version`, `config
show`, `projects list`) peaks at 20 MiB resident.

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

### 1. A supervisor per machine and user, an engine worker per CLI build and worktree

The machine process is two kinds of process:

- **One supervisor per machine and user.** It owns what is the machine's,
  whatever the build: the machine-wide execution flow (CPU and memory
  admission across every concurrent run of every worktree, section 5), the
  shared store and its collection, the file watching of every worktree
  (section 7), the toolchains under the Putnami home, and, once its own
  record lands, the extension worker pool (section 6).
- **One engine worker per CLI build and worktree in use.** It owns what
  depends on the build's code: the loaded workspace of the worktree it
  serves, the kept digests and witnesses of ADR 0113, the Git reader's state,
  the plans and the keys, and it runs the engine for its runs. A client is
  only ever served by a worker of its own build, so a CLI of another build
  never uses it.

**Amended 2026-11-27, by the owner: worktrees run in parallel.** A worker
runs one engine run at a time, so one worker per build made a second
worktree's concurrent request wait or run in its own process: measured on
two worktrees of this repository running the same unchanged request at once,
one was served by the worker while the other was refused every round and ran
in its own process. The supervisor therefore keeps a worker per build and
worktree, routes a request to the worker of its worktree, and starts one when
there is none, within the memory budget of section 10: before it starts one,
it stops idle workers, least recently used first, while the workers' held
memory and one more worker's would pass the budget. Two requests of one worktree share its files and its held
state, so they take turns: the second waits for the worktree's worker
rather than run in its own process. What the worktrees have in common is
computed once where it is keyed by content: the store and its in-flight
claims (a task key already executing for one run is awaited by the other),
the toolchains, the probe and activation witnesses persisted per program,
and, once its record lands, the extension pool. The supervisor stays the
single admission point that shares the machine's CPU fairly between the
concurrent runs (section 5).

```
 client (any build) ──unix socket──▶ supervisor (one per user)
                                      │ admission, watching, store, pool
                                      ├── worker, build A: worktree 1
                                      ├── worker, build A: worktree 2
                                      └── worker, build B: worktree 3
```

**No fixed cap on workers.** In this repository each worktree at another
commit builds its own CLI (`putnamiw` keys the binary by its sources), and
several worktrees run several streams of work at once. The supervisor starts
a worker on the first request of its build and evicts one by idle time
(section 2) and by its memory budget (section 10). An evicted worker costs
its next run a cold start, which is a run as today.

**Identities.** A worker is keyed by its build identity, its executable's
path and the root of the worktree it serves. The build identity (`fileindex.BuildIdentity`) is the SHA-256 of the
executable's build information and Go build ID, the parts of the file
index's program identity (ADR 0009 §4 of `protocols/workspace`) that do not
depend on where the file lies. A client reads it from the image it runs
(`/proc/self/exe` on Linux), so a client whose file was replaced since it
started is not served by the new file's worker. A CLI whose identity is empty
(no usable build ID) never uses a worker. The supervisor is identified by the
protocol versions it speaks (section 3), not by its build.

### 2. Discovery, start and stop

**Discovery.** The supervisor's directory is, in order:

1. `$XDG_RUNTIME_DIR/putnami` on Linux when the variable names an existing
   directory owned by the user with mode `0700` (a per-user tmpfs that the
   session manager empties at logout);
2. `$TMPDIR/putnami-<uid>` on macOS (`$TMPDIR` is the user's private
   `/var/folders/…/T/`);
3. `~/.putnami/machine` otherwise.

The directory is created with mode `0700` and used only when the user owns it
and no other user can write it, checked with `lstat` (a symbolic link is
refused). In it, `supervisor.sock` is the socket, `supervisor.lock` the
liveness lock and `supervisor.json` the description (protocol versions,
process id, start time, workers, worktrees), written after it listens. A
socket path longer than the platform's limit (108 bytes on Linux, 104 on
macOS, the limit `hostrpc` already handles) falls to the next directory; none
fits, no supervisor.

**Start.** A run that finds no supervisor runs in its own process, as today,
and starts one when it ends: its own executable as `putnami __machine
supervise`, detached in its own session (`proctree.StartDetached`, `setsid`),
standard streams on `/dev/null`, log at `supervisor.log` (rotated at 4 MiB).
The start never delays the run that asks for it. Two starters serialize on
`supervisor.lock`, which the supervisor holds exclusively for its life
(`flock`): a starter that cannot take it knows one lives, and one that can
removes a stale socket first. The supervisor then asks a worker of the
starter's build to load the starter's worktree.

A **worker** is started by the supervisor, never by a client: the client's
request names its executable's path and identity; the supervisor reads that
file's identity again, starts it as `putnami __machine work` with one end of
a socket pair as its only channel, and refuses a file whose identity differs
from the one claimed. A worker serves one executable file: two requests of
one build from the same file share one worker, and two files of one build get
two, since a source workspace pins the file its engine runs from, not the
build (found by the first implementation: two `putnamiw` blobs of identical
content were served by one worker, and the workspace refused the other's run).

**Stop.** A worker stops after 1 hour with no run, when the supervisor evicts
it for memory, when its executable's path no longer names the file it started
from (a rebuilt or removed CLI cannot start the same code again for its
nested calls), or with the supervisor. The supervisor stops after 3 hours with
no worker and no request (Bazel's and Gradle's default idle time), on
`putnami machine stop`, when it is replaced (section 3), or when it cannot
bring its workers under its memory budget. A stopping process finishes its
runs, refuses new ones (their clients run in their own process), removes its
socket and description, and exits.

**Crash: a run never fails because of the machine process.** A worker that
ends during a run (a panic, a kill) does not end the command. The supervisor
holds a copy of the client's connection while the worker serves it; on Linux
it is the processes' subreaper, so what the run had started becomes its
child when the worker ends, and it kills those processes and their groups.
It then tells the client, which runs the command again in its own process
and says so. Only a run the user had already interrupted keeps the
interrupted exit code. Because a command can run twice this way, `publish`,
`deploy` and `release` never go through the machine process. On macOS, which
has no subreaper, what the run had started outlives its worker, as it
outlives a CLI that ends during a run. A supervisor's crash closes its
workers' channels, and its workers exit; their clients run their commands
again in their own process, and the next run starts a new supervisor when it
ends. Whenever a command meant for the machine process runs in its own
process, the client records why, and `putnami machine status` shows it.

### 3. Two protocols, versioned and stable across builds

The supervisor serves clients and workers of builds other than its own, so
both of its protocols are versioned and change by addition only, as the
evidence protocol does (decision `evidence-version-1-changes-only-by-addition`):

- **client ↔ supervisor**: the handshake (protocol version, the client's
  identity and executable path), the run request routed to a worker, status
  and stop;
- **supervisor ↔ worker**: admission grants and releases (section 5), the
  change journal of the watched worktrees (section 7), memory reports and
  eviction, and the hand-off of a client's connection.

A protocol version is a number. A supervisor speaks every version from the
first stable one to its own; a member added in a later version is ignored by
an older reader, and a removal is a new protocol.

**A client or worker that needs a newer supervisor protocol** than the running
one gets the answer `machine.supervisor_outdated`. The client then runs in its
own process, and asks the supervisor to be replaced: the supervisor stops
taking new runs, and once no run is active it stops itself and its workers;
the next run, in its own process too, starts a supervisor of its own build.
Runs never wait for the replacement. A supervisor never replaces a newer one:
an older client is served by the newer supervisor through the version it
speaks.

**A rule change that makes stats skippable** is part of the protocol: the
journal's version names the watch rule it follows (section 7), and a worker
that requires a newer rule asks for a replacement as above and takes every
stat meanwhile.

### 4. A run through the machine process

**The worker runs the engine; the client is its front end.** A client sends
the supervisor its request: arguments, working directory, environment, umask,
the terminal's size and modes, and its standard input, output and error as
file descriptors (`SCM_RIGHTS`). The supervisor passes the connection itself
to the worker of the client's build, which runs the engine with exactly that
context, writes to those descriptors, receives the signals the client forwards
(interrupt, terminate, window size), and answers the exit code, which the
client exits with. A client that loses the connection mid-run reports it and
exits as an interrupted run does today; a worker cancels a run whose client
disconnected, as an interrupt does.

**A worker serves one run at a time, in its own process context.** For the
run, it sets the client's environment, working directory and umask on its
process and puts the client's descriptors on its standard streams (`dup2`),
then restores its own. The engine reads its context from the process as it
does today, so a run in a worker and a run in its own process read the same
values. A second run for a busy worker runs in its own process. Measured with
a throwaway build that ran four unchanged runs of this repository in one
process: each gave the same results as a first run (section 15). Taking the
context as a value instead (123 reads of the environment, the working
directory or the standard streams in 64 files of the engine) is needed only
for one worker to serve concurrent runs, and waits for a measurement that
shows concurrent runs of one build to be worth it. Memos that outlive a run
are reset before the next one or held under section 8's rule: the run resets
the toolchain probe answers as today, and the worker forgets what a CLI
process keeps for its life without a witness (the workspace load memo, the
once-per-process passes of the first-use bootstrap and of the lock-pinned
artifacts, the warnings a process prints once).

**Which runs go through it.** Every run a worker can serve the same way, and
none other. A client runs in its own process when:

- the machine process is turned off (section 12), or the client is in CI
  (`CI`, or the variable of a CI service that sets none: Azure Pipelines,
  Jenkins, TeamCity, CodeBuild, Buildkite…), or it is a hosted run
  (`--credential-fd`, ADR 0055), or a nested run (a `putnami` started by a
  job of a run, which carries `PUTNAMI_JOB_NAME`, or by the worker itself,
  which knows it without a variable that a key's env-input glob could
  read): a nested run through the machine process could wait on the run that
  started it;
- the command, read after its aliases resolve, is not a job command
  (`config`, `machine`, an extension's own command, `--version`…), or it
  watches (`--watch`, `-w`, or a serve workload, which watches by default),
  or it is `publish`, `deploy` or `release`, which a rerun after a crash
  could repeat;
- the client has a controlling terminal but its input or output is
  redirected: its jobs could reach that terminal only through `/dev/tty`,
  which a worker cannot give them;
- the worker's process differs from the client's in what a job inherits
  beyond the run's context: user and groups, resource limits, priority,
  control group, or the variables the Go runtime and the time package read
  once at start (`GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, `GODEBUG`,
  `GOTRACEBACK`, `TZ`);
- a run is active in the worker of its build (the second runs in its own
  process, with today's locks);
- the supervisor or the worker refuses it: stopping, outdated (section 3),
  over budget (section 10), or the identity check failed.

A refused or failed connection costs one `connect` and the run continues in
its own process: the machine process never stands between the user and a
run. The worker takes a request in two steps: it answers Ready, and runs the
command only on the client's go. A client that has no Ready within 15 seconds
(a supervisor that does not answer, a worker slow to start) sends no go and
runs the command itself, so a command never runs twice. The client forwards
an interrupt or a termination as such, and a worker whose client went away
stops the run as a termination does.

**A command at a person's terminal.** When the client's input and output
are a terminal, the worker opens a pseudo-terminal for the run, makes it the
controlling terminal of its session (each worker is a session leader with
none) with its own process group in the foreground, and puts its slave on
the run's standard streams. It passes the master to the client, which gives
it its terminal's settings and size, puts its own terminal in raw mode and
relays bytes both ways, and on SIGWINCH sets the new size on the master. The
run therefore meets the terminal as it would in its own process: the
renderer and its terminal detection are the same, a job's prompt reads
`/dev/tty` and turns echo off on it, Ctrl-C and Ctrl-\ reach the run's
foreground group through the pseudo-terminal's line discipline, and the
worker stops the run with the CLI's two-signal policy. Suspend is disabled
on the pseudo-terminal: the client sees the suspend key, gives its terminal
back and stops itself, as a command in its own process would stop; the run
goes on meanwhile, its jobs keep running and their output waits in the
pseudo-terminal until the shell resumes the client, which takes raw mode
and the size again. No job command has to run in its own process for a
prompt: an extension's own commands (`cloud login`…) and the structured
commands, which prompt in the CLI itself, never go through a worker. A
SIGTERM to the client is forwarded as a termination. A test drives a real
pseudo-terminal: the rendering, a resize during the run that its jobs read
on `/dev/tty`, and Ctrl-C during the run.

**What a run inside a worker does.** The same code as a run in its own
process, with held values in place of what the run would compute, each under
the witness rule of section 8. Its jobs are children of the worker, started
with the client's environment and in the client's working directory, as the
engine starts them today, once the supervisor has admitted them. A process a
run starts detached to outlive it (a cache collection, a session's delivery)
is waited by the worker in the background, since a child no process waits
stays a zombie of the worker for the worker's life; a run in its own process
releases it, and the process that adopts it when that run exits reaps it.
Its session is the same session in the same place. A session member that
says whether a run went through the machine process, and which held values
it used, comes with the held state; until then the client records why a
command meant for the machine process ran in its own process, and
`putnami machine status` shows it.

### 5. Machine-wide admission

Today each run admits its jobs against the machine's CPU capacity as if it
were alone (ADR 0100), so two runs of two worktrees each grant the whole
machine. The supervisor holds one budget for the machine: CPU, as ADR 0100
charges it, and memory, from the jobs' recorded peaks
(`executions[].maxRssBytes`) against the memory available. A worker asks it
for each grant and returns it when the job ends; concurrent runs share the
machine fairly by run, and a run alone gets what it gets today. A run in its
own process (in CI, or with the machine process off) admits as today: the
supervisor changes the scheduling of concurrent runs, never a key or a
result.

Measured on 2026-11-27 with two worktrees of `299343dc` on this container,
signed out, the same command as above in each:

| Scenario | Each run alone | Both at once |
|---|---|---|
| Unchanged | 24.4 to 28.1 s | 27.9 to 28.8 s |
| Small change, a different line appended in each worktree | 114.9 s (118.9 s earlier) | 190.9 s and 202.1 s |
| Peak one-minute load, small change | 8.11 | 12.38 |
| Run CPU (`runMs`), small change | 305.1 s | 306.4 s and 374.6 s |

An unchanged run uses about 1.4 cores (33 s of engine CPU in 24 s), so two
barely meet. Two small changes do: each run takes 1.7 times as long as alone,
the load reaches 3.1 times the cores against 2.0 alone, and the two spend
12 percent more CPU together than two runs alone, which neither run can see
from its own admission. The prototype measures what one budget changes
(section 15, scenario 4); it is expected to cut the load and the extra CPU,
not the total work. When both worktrees make the same change, the second run
is served by the first one's entries in the shared store (27.7 s); started
together, one computes and the other waits for it (126.9 s and 133.2 s).

### 6. What it holds

Held state lives in memory only. The machine process writes nothing a run in
its own process does not write; what a run writes to disk through a worker
(the file index, the input record of ADR 0113, the workspace snapshot, the
session) it writes as a run in its own process does, so a run without the
machine process finds the same files.

**In a worker, per worktree**, keyed by the worktree's root and the Git common
directory:

| Held value | What it replaces | Witness |
|---|---|---|
| The loaded workspace: merged configuration, extension discovery and placement, the probe view | workspace-load, runtime-sync's discovery, probe (0.8 to 1.1 s) | The workspace snapshot's inputs (ADR 0009 §7 of `protocols/workspace`) |
| The file index of ADR 0009 | its load and save | Each file's stat |
| The input record and release baselines of ADR 0113 | the record's load, and each witnessed stat | ADR 0113 §1 to §4 |
| The Git reader's state of ADR 0112: parsed index, HEAD, refs, configuration, the cut | the engine's Git calls (17; on a full clone, those ADR 0113 leaves) | The index stamp, the files of `.git` it read, the environment it read |
| The last plan per request shape (command, selection, options, parameters, the environment's digest) | plan (1.6 to 1.8 s) | Every value the plan read: the loaded workspace, the configuration, the request |
| Each task's last key, its inputs' identities, and the outputs it left on disk | composing a key whose inputs are the same values; checking outputs untouched since | The inputs' own witnesses; the outputs' paths, watched since the run that wrote or checked them |

A held plan or key is a memo of a function the run computes anyway: it is
used only when every value it was computed from is the same held value, or
equal to it. A request whose environment digest differs plans again.

**In a worker, for its build**: the runtime-sync answers (the runtime-info
handshake of ADR 0102) under the witness of the runtime's files, and the
toolchain probe answers of ADR 0102 and 0105 under the stat of the probed
binary (today they are cleared at every run start).

**In the supervisor, for the machine:**

- **The store.** Its root and lock, not its content: a cache entry is read
  from disk. The supervisor runs the store's opportunistic collection
  (`store.MaybeOpportunisticGC`, [Caching](../10-caching.md)) when no run is
  active, so a run no longer pays it at its end.
- **Toolchains.** Their files stay under the Putnami home; the supervisor
  serializes their provisioning across concurrent runs. It starts no language
  server or build daemon (`gopls`, `tsserver`, the Gradle daemon): a tool's own
  daemon is the tool's choice, started by its job.
- **The extension worker pool: owned here, decided in its own record.** Every
  job is a process of its own today (one per task, one per batch), and an
  extension serves no second job. A pool needs an extension protocol in which
  an extension process serves several jobs, which `protocols/workspace`
  ADR 0001 rejected for providers. On an unchanged run, extension CPU is 0.8
  to 1.3 s for 51 to 60 job processes, which the uncached-task work removes;
  on the small change it is 33.1 to 35.7 s for 621 job processes, and on a
  cold run sdd and go spend 56 s and 42 s, which separate work on those
  extensions reduces first. The pool's record is sized on what that work
  leaves; this record fixes only that the supervisor owns it, since a pool
  serves every build whose extensions match.
- **Session delivery, optional.** ADR 0115 hands a signed-in run's report
  upload to a detached replay so the run does not wait for it. The supervisor
  may later own that delivery, one queue for the machine, holding a
  reporter's token for that upload only, in memory, until it ends or fails.
  Without the machine process, ADR 0115 applies as it stands.

### 7. How it learns of changes

**One watcher, in the supervisor**, one watch set per worktree, shared by every
worker that serves it. On Linux, one `inotify` instance (the user has 128),
one watch per directory: the directories the default walk enters (the skip
list of ADR 0113 §1), the directories of every witnessed path, `.git` and its
`refs` directories, the configuration files' directories, and the toolchain
and extension-runtime directories whose answers a worker holds. macOS has
no watcher in this implementation: FSEvents needs cgo, which the CLI is built
without, so every path is taken by stat there, which costs what a run in its
own process pays and skips nothing.

**A change journal.** Per worktree, the supervisor keeps a sequence number
and the paths that changed since a worker's last read, or a break marker for
a subtree or the whole worktree. A worker that holds a worktree's state at
sequence `n` asks for the journal from `n` at the run's sync point; paths in
it are taken by stat, a break makes everything below it taken by stat, and
the rest holds. The journal is the supervisor ↔ worker protocol's, so a
worker of any build reads it.

**A sync point before each run.** The supervisor writes a cookie file in the
worktree's `.putnami/` (watched, not walked) and waits for its own event:
every write that completed before the cookie was written has been delivered
by then (Watchman's method; on macOS `FSEventStreamFlushSync` first). A sync
point not reached in 2 s is a break.

**A path holds without a stat only as ADR 0113 §7 says**: watched, or the
directory that holds it watched, without a break since the worker last took
its stat, on a file system ADR 0113 §2 trusts. Each of these is a break:

- a queue overflow (`IN_Q_OVERFLOW`; on macOS `MustScanSubDirs`,
  `UserDropped`, `KernelDropped`): the whole watch set;
- a watch removed (`IN_IGNORED`, an unmount), a watched directory renamed or
  removed (`IN_MOVE_SELF`, `IN_DELETE_SELF`): below it;
- a directory created: watched from the event on, and every path below it
  taken by stat until a stat after its watch was added;
- a watch the kernel refused (`ENOSPC` at `max_user_watches`): that directory
  and everything below it is unwatched and taken by stat;
- a sync point not reached, a supervisor or worker restarted: every path, once;
- a file system ADR 0113 §2 does not trust (NFS, SMB, 9p, FUSE, FAT), and
  Windows: never watched, no record anyway.

**The limits.** `max_user_watches` is shared with every other watcher of the
user (editors, other tools): the supervisor uses at most half of what is left
when it starts, read from `/proc/sys/fs/inotify/max_user_watches` and its own
count; a worktree that does not fit is watched for its most recently used
directories, the rest taken by stat. This repository needs about 3,000
watches; the kernel charges each against the user's memory (about 1 KiB at
worst on 64-bit). macOS coalesces FSEvents and may report a directory instead
of a file: an event for a directory is a break below it, which costs stats,
never a missed change. A write through `mmap` raises no event until the file
is closed or its times move, the risk ADR 0009 already names for a stat;
overlayfs reports writes made through the overlay, and a write to a lower
layer under a mounted overlay is unsupported by the kernel.

**A sampled self-check.** At each run the worker also takes the stat of 64
paths it would have answered without one, chosen at random. A stat that
differs from what the journal says is a break for every worktree, is logged
and counted in `putnami machine status`, and turns stat skipping off in the
supervisor for the rest of its life: every later run takes every stat, still
served warm otherwise. A watcher bug shows as stats, never as a stale key.

### 8. It accelerates and never decides

Every held value is answered under a witness, the witness is the one a run in
its own process checks, and a worker may skip a check only by ADR 0113 §7's
rule. A held value whose witness the machine process cannot vouch for is
computed again. Hence:

- a run in its own process, in CI or on a cold machine, computes the same
  keys, plans the same tasks, and gets the same results and exit code;
- no key, entry address, record format or protocol value changes, and nothing
  the machine process holds is written as a value another run trusts.

**A comparison test checks it.** A test of `internal/cli/e2e/machineproc` starts a
supervisor on a Git fixture and runs the same commands through it and in
their own process (`PUTNAMI_MACHINE=off`), after each round of the randomized
writes of ADR 0113 §5 and a branch switch, a stage, a configuration edit and
an extension runtime change, and compares the plan, every task's key, every
task's outcome and the exit code. A difference fails the test. It runs again
with a watcher that drops events at random and reports the drops as
overflows, with one that reports none of a round's events while every stat is
forced, and with two workers of two builds on two worktrees at once. The
orchestration counts tests (`internal/cli/e2e/orchestration`) run both ways.

CI never starts a machine process (section 4), so no CI result depends on one.

### 9. Versioning

A client is served only by a worker of its own identity: the supervisor
routes by identity, and the worker refuses a mismatch with
`machine.identity`, after which the client runs in its own process. A worker's
held state never leaves it, so no state crosses builds. What crosses builds
is the two protocols of section 3, versioned and additive; a supervisor too
old for a client is replaced when idle, never used past its version.

### 10. Memory

**The budget is the supervisor's**: at most 1 GiB, or one eighth of the
machine's memory if that is less, for what the machine process holds between
runs, all workers together. Each worker reports its held memory after each
run; past the budget the supervisor drops the least recently used worktree
from its worker, then stops idle workers, least recently used first. A dropped
worktree or a stopped worker costs its next run a cold start.

**A run's memory is admitted like its CPU** (section 5): a worker asks for its
run's expected peak, the peak of that worktree's last run, before it starts.
A run on this repository peaks where a run in its own process does: 209 to
234 MB of live heap and 663 to 801 MiB resident at the 200 percent target of
ADR 0111. A run the machine's available memory cannot admit runs in its own
process. A worker sets the Go runtime's soft limit (`debug.SetMemoryLimit`)
to its held state plus its admitted runs, and returns a run's heap to the
system once its results are written (`debug.FreeOSMemory`), so an idle worker
keeps about its held state.

What one worktree costs held, and what each process costs: once planned, this
repository's engine keeps 44 to 51 MB of live heap, and the record of
ADR 0113 is about 2.8 MB on disk; a CLI process that loads nothing peaks at
20 MiB resident (`putnami --version`, `config show`, `projects list`). The
prototype measures a worker's held memory per worktree (section 15).

### 11. Security

- The socket lives in the directory of section 2, owned by the user, mode
  `0700`, and is itself `0600`. No TCP port, no loopback HTTP. A worker has no
  socket of its own: its only channel is the socket pair the supervisor
  started it with, and the client connections the supervisor hands it.
- The supervisor accepts a connection only from its own user: it reads the
  peer's credentials (`SO_PEERCRED` on Linux, `getpeereid` on macOS) and
  closes any whose user id differs. Root talks only to root's supervisor, and
  a user's CLI never to root's.
- The supervisor starts as a worker only an executable whose identity it read
  itself and that matches the request, owned by the user or by root and not
  writable by another user.
- Same-user is the boundary, as it is for the store and the file index today:
  a process of the same user can already read the user's files and trace the
  user's processes.
- A run's environment, credentials included, is held for that run only and
  dropped when it ends; nothing of it is logged. Held state keeps environment
  digests, never values. A hosted run, whose credentials ADR 0055 keeps out of
  repository processes, never goes through the machine process.
- A job of a run inside a worker carries `PUTNAMI_MACHINE_RUN`, and a CLI that
  sees it runs in its own process: a job reaches no other worktree through the
  machine process.

### 12. Settings, default and kill switch

- **Off by default until the owner decides the default on measurements.**
  The owner first set it on for Linux and macOS on 2026-11-27; the machine
  process ships off, `putnami machine on` or `PUTNAMI_MACHINE=on` turns it on,
  and the default changes only by the owner's decision on the measurements of
  section 15, on Linux and on macOS. Always off in CI (`CI` set), in hosted
  runs and in nested runs, whatever the setting. Windows has no machine
  process (decision `windows-support-is-suspended`).
- **Setting**: `putnami machine on|off` records the user's choice in
  `machine.json` under the Putnami home, as `putnami telemetry on|off` does
  for telemetry: the switch is the user's, for every workspace.
- **Kill switch**: `PUTNAMI_MACHINE=off` turns it off for one command, the
  setting for the user (`putnami machine off` also stops the running
  supervisor), `putnami machine stop` stops the supervisor and every worker
  once no run is active. A user who turns it off loses no result: every run runs in
  its own process as today.
- **Commands**: `putnami machine status` (the supervisor, its protocol
  versions, its workers and their builds, worktrees, held memory, watches,
  breaks, self-check results, last stop reasons), `putnami machine stop`.

**The macOS risk.** This epic measures nothing on a Mac. What is unproven
there: the descriptor and signal hand-off, `getpeereid`, the socket pair
without `SOCK_CLOEXEC` (made under the fork lock), and the memory figures;
the CLI is compiled for darwin, never run there. What bounds the risk: the
machine process is off by default, so a Mac runs it only once its user turns
it on; macOS has no watcher, so it skips no stat (section 7); a run whose worker ends runs
again in its own process; the comparison test runs on that Mac with the
first measurement; and the kill switch above leaves runs exactly as they are
today. The first macOS measurement of
the protocol of section 15 is a task of the implementation, before the owner
decides the default.

### 13. The refusal of a background program is changed

`protocols/workspace` ADR 0009 rejected "a background program that watches the
tree" because one per worktree does not fit many worktrees and it gives
nothing to CI or to a cloud session. This record keeps both facts and changes
the conclusion: one supervisor per machine and user serves every worktree,
and a run without it, in CI or in a cloud session, is a run as today. ADR
0009's rejected alternative is amended to point here, and the decision
`a-machine-process-serves-every-worktree` records it. The other rejections of
a daemon stand: the registry credential is still not served by a socket
(ADR 0101, registry ADR 0001), and an extension provider is still not a daemon
(`protocols/workspace` ADR 0001) until the pool's record changes it.

### 14. A supervisor, or one process per worktree

The simpler design is one process per worktree: no supervisor, no protocol
across builds, the worktree's own build always. What decides between them:

| | One process per worktree | Supervisor and a worker per build |
|---|---|---|
| Held state of one worktree | 44 to 51 MB live heap and the record (2.8 MB) | the same |
| Fixed cost per process | about 20 MiB resident, once per worktree | once for the supervisor, once per build in use |
| A new worktree's first run | cold: 54.2 s here, 30 s of it the implicit install | cold for its own state; runtime answers and toolchain answers warm when its build has a worker |
| Concurrent runs of two worktrees | each admits the whole machine: two small changes take 1.7 times as long as alone, at 3.1 times the cores (section 5) | one budget for the machine |
| Watching | one `inotify` instance per worktree, of the user's 128, shared with editors | one instance |
| Extension worker pool | one per worktree, or none | one, shared |
| A CLI build change | the worktree's process restarts | a new worker; the supervisor stays unless its protocol is too old |
| Protocols to keep stable | client ↔ process, same build only | client ↔ supervisor and supervisor ↔ worker, across builds |

Memory alone does not decide: held state is per worktree either way, and the
fixed cost the supervisor saves is about 20 MiB per worktree beyond the first
of each build. Warm-up alone does not decide either: a new worktree's first
run is cold in both, dominated by the install. What decides is the machine's
execution flow: only a process that sees every run can admit them against
one machine, own one pool and one watcher, and the measurement of section 5
shows what two runs that each admit the whole machine cost.

**Recommendation: the supervisor with a worker per build**, built in this
order so the simpler design is a step of it, not a fork: the prototype starts
one worker behind a supervisor that only routes and watches (section 15); the
admission of section 5 follows; the pool comes with its own record.

### 15. Prototype plan and measurement protocol

Before the implementation, a prototype on a branch of its own, measured and
then thrown away or folded into the first slice:

- a supervisor that routes by identity, watches on Linux with the journal,
  sync point and break rules of section 7, and starts workers;
- a worker that runs the engine in-process with the client's descriptors and
  environment, holding the loaded workspace, the file index, the input record
  (once ADR 0113 lands; before, the in-run memo of ADR 0109 kept across runs
  under the journal), the Git reader's state and the plan;
- the comparison test of section 8 on the fixture, and on this repository's
  unchanged run (the keys and outcomes of the runs in their own process and
  through the worker compared one by one).

**Protocol.** This container class: 4 vCPU Linux, ext4, signed out, the
current commit of main and the prototype's branch, on a shallow clone and on
a full clone with tags. Each scenario runs once to prime, then three times
with the machine process warm and three times with `PUTNAMI_MACHINE=off`,
medians reported:

1. **Unchanged run**: `./putnamiw lint,test,build,validate --all
   --enforce-coverage` on the unchanged tree.
2. **Small change**: the same command after appending a new comment line to
   `go/framework/client/binary_call.go` before each run.
3. **Second worktree**: `git worktree add --detach` at the same commit, then
   the same command in it, first run and second run, with the supervisor
   already serving the first worktree.
4. **Two worktrees at once**: the unchanged run in two worktrees started
   together, through the supervisor and in their own processes.

Measured each time: wall of the `./putnamiw` call and of the binary called
directly; engine CPU (in its own process, `engineMs`; through a worker, the
worker's CPU over the run); engine Git calls; job processes; key, restore and
ready-wait times; resident peak of the client, the supervisor and the worker;
the worker's held memory per worktree after the run; the watch count and
setup time; the breaks and self-check results. The same protocol runs once on
a Mac before the owner decides the default (section 12).

**Expected, and what the prototype must show.** Through a worker the 2.8 s
before execution and the 17 Git calls go to about nothing. The target, under
2 s for an unchanged run, also needs the uncached-task work (no job process on
an unchanged run) and the per-task fixed-cost work (no per-task fixed cost on
a hit whose outputs are on disk): with them, each of 2,245 hits costs the held
key's check and its session record, which must stay under 0.5 ms of CPU each
(1.1 s for 2,245 on four workers), and the 34 MB session must be written in a
fraction of a second. `./putnamiw` adds 151 to 169 ms of its own. The
prototype reports each part; a target it cannot reach is reported with the
part that holds it.

**Measured: what a warm process holds today.** On 2026-11-27, a throwaway
build ran the CLI's `run` four times in one process (full clone with tags,
signed out, the unchanged command above), so runs two to four found the
process warm with every memo the code keeps today:

| Runs two to four in one process | Wall each | Summed key time |
|---|---|---|
| Today's memos only | 27.0 s, 27.4 s, 27.8 s (the first: 27.7 s) | 14.1 to 15.3 s |
| Plus the run's input-digest memo kept across runs, with no witness | 25.2 s, 25.6 s, 26.4 s | 11.8 to 14.9 s |

Every phase is as long in a warm process as in a new one (workspace-load 57
to 102 ms, runtime-sync 209 to 363 ms, probe 773 to 972 ms, plan 1.7 to
1.9 s, 512 Git calls): no memo of today's code survives a run with a value
the next run uses. Keeping the digests with no witness saves 1.5 to 2.5 s, a
bound, since the run's own writes still drop many of them. A warm process
therefore saves what it holds under section 8's rule, not what it is: the
held record of ADR 0113, the plan and the Git reader's state, then the
per-run constant left once the uncached-task and per-task fixed-cost work
have landed.

**Measured: the first slice.** On 2026-11-27, the supervisor, the worker
per executable and the client, with no held state beyond today's, on this
protocol (full clone with tags, signed out; medians of three, or each run):

| Scenario | Through a worker | In its own process |
|---|---|---|
| Unchanged | 24.0 s (23.4 to 24.6) | 23.7 s (22.5 to 24.4) |
| Small change, alternating which goes first | 106.4 s, 101.6 s | 101.8 s, 102.1 s |
| Second worktree, first run and second run | 42.9 s, 28.3 s | 40.1 s, 24.6 s |
| Engine Git calls, unchanged | 512 | 514 |
| Resident peak of the process the user started | 21 to 23 MiB (the client) | 609 to 632 MiB |

The worker peaks at 645 MiB during a run and keeps 66 MiB resident, 28 MiB of
heap in use, between runs; the supervisor keeps 20 MiB. A run through a
worker costs what it costs in its own process, as the in-process measurement
above predicted: the slice changes where the engine runs, not what it
computes. A small change measured once with the worker first took 138.4 s
against 109.9 s: the first run after a pause also rebuilt Go's own cache, and
the alternating pairs above show no difference.

**Order of the implementation.** First, the supervisor, the worker per
executable, the client, `putnami machine` and the comparison test of
section 8: a run through a worker gives the same results, with no held state
beyond today's. Then, once ADR 0113's record has landed, the watcher of
section 7 and the held state of section 6, measured on this protocol at each
step. Then the admission of section 5 and the memory budget of section 10.

## Consequences

- An unchanged run through a worker makes no Git call, starts no process of
  its own before its first job, and plans nothing it planned before.
- Concurrent runs of several worktrees share one machine budget instead of
  each admitting the whole machine.
- A new worktree starts with its build's runtime and toolchain answers warm
  when a worker of that build lives, and its own state cold.
- A worker applies a run's context to its own process and serves one run at
  a time; serving concurrent runs of one build would take the context as a
  value, a refactor of the 64 files that read it.
- Two protocols are kept stable across builds, by addition only.
- A worker can outlive its build's last run by an hour and the supervisor its
  last worker by three, within the memory budget; `putnami machine status` and
  `stop` show and end them.
- `inotify` watches count against the user's limit, shared with editors.
- A run through a worker starts its jobs from the worker, not the client's
  shell: they see the client's environment and descriptors, but their parent
  is the worker. A tool that reads its parent's identity sees
  `putnami __machine work`.
- The machine process is off by default until the owner decides the default
  on measurements (section 12); a user who turns it on before then opts in.
  macOS is not measured in this epic; section 12 states the risk and the kill
  switch. Windows has none.

## Rejected alternatives

- **One process per worktree.** Compared in section 14: as cheap in held
  memory, as cold for a new worktree, but blind to the other worktrees' runs,
  so it cannot hold one budget for the machine, one watcher or one pool.
- **One process per user and build, at most two**, as this record first
  proposed. A cap of two goes against working several streams at once in
  several worktrees, and it puts the machine's execution flow in a
  build-specific process; the owner rejected it on 2026-11-27.
- **Strictly one process per user, whatever the build.** Two worktrees at two
  commits of this repository run two CLI builds; a single process would be
  stopped and started at each alternation.
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
- **Polling, as `internal/watch` does for `watch` mode.** A poll of this tree
  is the stat of every witnessed path (28 ms, ADR 0113), which a run without
  the machine process already pays once; a poll every 500 ms costs it 7,200
  times an hour for an idle supervisor.
- **A pool of extension workers now.** It changes the extension protocol for
  0.8 to 1.3 s of extension CPU on an unchanged run, which the uncached-task
  work removes. Its gain is on small-change runs (33.1 to 35.7 s of extension
  CPU, measured above) and cold runs, whose extension CPU the work on sdd and
  go reduces first; the pool's record is sized on what that work leaves.
- **The machine process persists its held state to disk.** That is ADR 0113's
  record, which a run in its own process already writes; a second on-disk form
  would be a second thing to keep correct.
