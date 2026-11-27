# ADR 0009: A file's stat decides that it is unchanged

- **Status**: accepted
- **Date**: 2026-11-25
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): amends
  ADR 0001 §4; `@putnami/cli`: the workspace snapshot
  (`.putnami/workspace-index.json`) and the input hashes of cache keys
- **Amended**: 2026-11-25, decision 7: the index keeps the bytes of the
  configuration files a run parses
- **Amended**: 2026-11-27, rejected alternative "A background program that
  watches the tree": one supervisor per machine and user, shared by every
  worktree, is decided by `@putnami/cli`
  [ADR 0116](../../../../tooling/cli/doc/adr/0116-one-machine-process-keeps-every-worktree-warm.md)

## Context

ADR 0001 §4 let stat data prioritize work and nothing more: only a content
digest decided validity. The CLI read that as "read every file again". The
workspace snapshot was declared valid by hashing every recorded input, and the
input digests of cache keys lived in memory for one run. No digest survived a
run, so a run of an unchanged tree read the content of every input it keyed.

Measured on 2026-11-25 at `fe1a0430`, where the scheduler already computes a
job's batch key once, on a 4-core Linux container (Intel Xeon @ 2.10 GHz),
`lint,test,build,validate --all --enforce-coverage` run again on the tree of a
complete run:

- Wall: 81.4 s for 2,212 selected tasks, 2,064 of them served from the local
  cache.
- Engine phases: workspace-load 43 ms, runtime-sync 1,310 ms, probe 1,090 ms,
  selection 60 ms, plan 1,976 ms; then the run itself, where computing the
  cache keys took 56,450 ms summed over the tasks.
- Engine CPU: 190,035 ms of the run's 220,730 ms.
- Files the engine process opened for reading, counted by `strace -f` on a
  second unchanged run (its children excluded): 2,582,170. The most opened:
  `putnami.json` 68,357 times, `README.md` 56,807, `putnami.features.json`
  32,942, `go.mod` 26,243. The 2,562,401 opens of files of the workspace
  itself fell on 16,701 distinct files.

The repository tracks 12,975 files, 89 MB. Reading them all once takes 73 ms
from the page cache; taking the stat of all of them takes 18 to 23 ms.

## Decision

### 1. A machine-local index maps a file's stat to its content digest

`.putnami/file-index` maps a file and a reading of it to the stat the file had
when it was read and the digest read. The stat is the device, the inode, the
mode, the owner and group, the size, and the modification and status-change
times to the nanosecond. A reading names how the digest was taken (the raw
bytes, a project config decoded under a scope, a build stamp without its
invocation fields, a Git candidate with its mode), so two readings of one file
never answer for each other.

A file whose stat equals the recorded stat is unchanged, and the recorded
digest answers for its content without reading it. The digest itself is what
it was: a cache key and a snapshot hold the same values as before, and no
digest, stat or timestamp enters the protocol.

### 2. A file written in the same tick as the index's clock is read again

This is git's racy-clean rule, made strict. Before a run that records reads
any file, it reads the filesystem's own clock: it creates a file beside the index and takes
its modification time. An entry is recorded only when both of the file's times
are older than that clock by more than two seconds, twice the coarsest tick a
listed filesystem keeps (one second on ext3 and HFS+). A later write then moves
the modification time past the recorded one, whatever the tick. A file written
in the same tick as the clock, or in the two seconds before it, has no entry,
and the next run reads it again.

An entry is also recorded only when the file's stat before and after the read
are equal. A file written while it was read has no entry.

### 3. What the index never trusts

- A file that is not a regular file. A symbolic link is read through, every
  time.
- A file on another device than the index: its clock was not read.
- Every file, when the index's directory is not on a listed local filesystem
  (ext2/3/4, XFS, Btrfs, tmpfs, ZFS, F2FS, bcachefs and overlayfs, whose
  layers are local disks in practice, on Linux;
  APFS, HFS+, UFS, ZFS and tmpfs on macOS and FreeBSD). On NFS, SMB, 9p and
  FUSE a stat can be served from an attribute cache another host made stale,
  and FAT keeps no status-change time.
- A file whose times lie ahead of the clock.
- A relative path.
- Every file on a platform whose stat the CLI does not decode (Windows, and
  every platform but Linux, macOS and FreeBSD): it has no inode and no
  status-change time to compare.

### 4. A damaged index costs reads, never a stale digest

The file names the program that wrote it, read once when the process starts:
a reading-scheme version compiled into the CLI, the CLI's build information
(module versions and sums, VCS revision), the Go build ID of the executable
the process runs, found in its first 64 KiB, and that file's path and stat (on
Linux, `/proc/self/exe`). Two builds that differ in code have different Go
build IDs, so an index another build wrote loads empty, and a CLI that
changed how it reads a file, or fixed this rule, never answers with a digest
an older one recorded. A golden test pins the digest of each reading under
its reading-scheme version. A process whose executable cannot be read, or
carries no usable build ID (none, an empty one, or one a build redacted with
`-buildid=`), uses no index; so does one whose build ID lies beyond the first
64 KiB, such as an externally linked Mach-O might, which `--debug` shows as no
digest answered. The rule relies on the build ID cmd/go assigns from a
build's inputs: a build ID set by hand with `-ldflags=-buildid` lies outside
it.

The file ends with the SHA-256 of what precedes it. A file that does not
parse, carries another format version, or fails its checksum loads as an empty
index. An index whose stamp, the newest clock a run that wrote it read, lies
ahead of the clock discards itself: the clock went back, and an entry could
match a file written at its recorded time. A run whose clock went back while
it ran records nothing.

### 5. Concurrent runs share one index

Each run loads the index, records what it read, and at its end merges into
the file on disk under an exclusive lock, then renames a complete file into
place. An entry is decided when it is recorded, so a merge cannot make a racy
entry trusted. Entries this run found out of date are dropped from the merge,
and so is an entry no saving run recorded or confirmed in its last 32; the file
holds at most 262,144 entries. A run that only confirms entries writes nothing,
until they come halfway to being dropped.
`--plan` and a preview-only `--dry-run` read through the index with the
process clock and write nothing: no directory, no clock file, no entry.

### 6. ADR 0001 §4, amended

The digest stays canonical and content decides validity. A content digest the
CLI compares may be answered from this machine's file index under the rule
above. The protocol still carries no timestamps and no file sizes.

### 7. The bytes of a configuration file are kept under the same rule

Measured on 2026-11-25 on the same container, `strace -f` on the engine
process of an unchanged `lint,test,build,validate --all --enforce-coverage`
run once the digests above answered: the engine opened `putnami.json` and
scope files about 880 times, every run, to parse them again;
`protocols/putnami.json` alone 98 times in a run of one project. A digest does
not help a parser, which needs the bytes.

The index therefore holds a second kind of entry: a file, a reading, its stat,
and the file's bytes with their SHA-256. A file whose stat equals the recorded
stat answers its recorded bytes without being read. Every rule above applies
unchanged: the margin, the stat before and after the read, what is never
trusted, the program identity and the checksum. Such an entry is also
recorded only for a file of at most 64 KiB whose size is the length read,
and an entry whose bytes do not have its SHA-256 or its stat's size loads
the whole index as damaged. The configuration loaders of this module read
through a reader their caller sets (`UseConfigReader`); with none, they read
every file; the CLI's planner reads a file an activation gate searches the
same way. A content entry and a digest entry of one file are two readings
and never answer for each other. A process reads its configuration before it
knows whether its run may write, through the index opened read-only; the
index of a run that records reads the files it read again, under its own
clock, to record them.

Rejected: keeping the decoded configuration rather than its bytes. A decoded
value written back loses what its decoding keeps of the authored file (an
empty list against an absent one, a raw member), so a configuration could
load otherwise from the record than from the file. Decoding kept bytes is
the same decoding; it costs well under a millisecond for this workspace's
files.

## Rejected alternatives

- **A background program that watches the tree.** One per worktree does not
  fit a repository worked through many worktrees, and it gives nothing to CI
  or to a cloud session, which start from nothing. With the index, what an unchanged run
  still pays for its files is one stat per input: 18 to 23 ms for every
  tracked file of this repository, the most a watcher could save.
  Amended on 2026-11-27: this rejected one program per worktree. One
  supervisor per machine and user, shared by every worktree, with an engine
  worker per CLI build, is decided by `@putnami/cli` ADR 0116 (decision
  `a-machine-process-serves-every-worktree`): it holds far more than the stats
  (the loaded workspace, the plan, the Git reader's state), a run without it
  is a run as before, and CI never depends on it.
- **Reading every file again (the rule this ADR replaces).** See the
  measurement above.
- **Size and modification time alone.** A modification time set back by a
  tool (`cp -p`, `tar`, `rsync -t`) with the same size would answer with the
  old digest. The status-change time and the inode cannot be set back by a
  tool, and they move on a rename over the file.
- **Git's own index.** It holds Git blob ids of tracked files only: ignored
  and untracked inputs, and every reading other than the raw bytes, would
  still be read. Refreshing it writes `.git/index` from a git process.
- **One index per machine, in the store.** Every worktree has its own
  modification times, so the entries would not be shared, and every run of
  every worktree would contend for one lock.
- **A tolerance of one tick instead of two seconds.** A stat does not report
  its filesystem's tick, so the margin is the coarsest one a listed
  filesystem can keep.
- **A memo of digests for one run.** It would remove most of the 2,562,401
  opens, which fell on 16,701 distinct files, and still read every input once
  per run; the index reads none.

## Consequences

- The first run in a new worktree reads every file once: its modification
  times are new.
- A file written less than two seconds before a run is read by that run, and
  recorded by the next.
- On Windows, every run reads every file, as before.
- Trust in the index is trust in the machine: a process that sets a file's
  times and inode back, by changing the system clock, is outside the rule, as
  it is for git.
- Risks git shares remain. A write through a memory mapping updates the
  file's times at the kernel's next page fault or `msync`, not at the write,
  so a mapping written after the read can show the old stat for a while. A
  filesystem mounted `lazytime` keeps exact times in memory, but after a crash
  the times on disk can be older than the content.
- A single long in-place `write(2)` is a third such risk: the kernel sets
  the file's times when the write starts, so one that started more than two
  seconds before the run, keeps the file's size and is still copying while
  the run reads the file leaves a stat that matches the partial content the
  run records, until a later write moves the file's times again. Git has the
  same window.
- A big-endian ELF executable (s390x, ppc64, mips, mips64) writes its build
  ID note header big-endian, which the identity does not decode, and the Go
  linker writes no build ID text marker into an ELF file: such a CLI has no
  identity and runs without the index, reading every file as before.
