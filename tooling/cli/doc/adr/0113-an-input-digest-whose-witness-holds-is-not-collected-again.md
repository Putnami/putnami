# ADR 0113 — An input digest whose witness holds is not collected again

- **Status**: proposed
- **Date**: 2026-11-26
- **Amends**: [ADR 0109](0109-a-write-drops-a-memoized-digest-only-when-it-can-move-it.md)
  (its rejected alternative "Keep the digests across runs" becomes this
  decision), and extends the rule of `protocols/workspace`
  [ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md)
  from a file to a directory
- **Depends on**: ADR 0112, "The engine reads the Git index and the work tree
  itself" (proposed beside this one), whose in-process reader gives the cut's
  witness (section 3)
- **Scope**: `@putnami/cli` (`internal/store`: the input digests of cache keys;
  `internal/fileindex`: the record's envelope; `internal/jobs/cachekey`: the
  key inputs and the prune precompute; `internal/git`: the cut's witness)

## Context

ADR 0009 made a file's stat answer for its content digest across runs, and
ADR 0109 kept an input digest for the rest of a run once its witness, the
files its patterns selected, holds against the run's writes. No input digest
survives a run: each run walks every directory its patterns name, matches
every pattern and folds every file's digest again, even when no file changed.

Measured on 2026-11-26 at `ef6c1aa2` (main), on a 4-core Linux container
(ext4 root), with the protocol of the earlier unchanged-run measurements: `./putnamiw
lint,test,build,validate --all --enforce-coverage` run once cold to prime the
caches, then three times on the unchanged tree, signed out of Putnami Cloud
(signed in, the session reporter's upload adds about 57 s after the session
ends, outside this decision). Medians of the three unchanged runs:

| Measure | Value |
|---|---|
| Wall | 30.7 s |
| Engine CPU (`run.orchestration.cpu.engineMs`) | 42.7 s |
| Longest ready wait | 11.8 s |
| Key time summed over the tasks (`tasks[].orchestration.keyMs`) | 20.0 s |
| Engine Git calls | 130 |
| Selected tasks, all served from the local cache but 57 job processes | 2,239 |

A CPU profile of the engine over two of those runs (`runtime/pprof`, local
build) puts 11.6 to 12.1 s of the engine's CPU under
`ComputeJobCacheHashWith`, of which 9.7 to 10.5 s under
`CacheManager.lookupFileHash`: collecting and folding input digests
(`filepath.WalkDir` and `os.ReadDir` 6.7 to 6.8 s, `os.Lstat` about 5 s of
it, the file index 3.4 to 3.6 s, pattern matching 2.5 s). The prune
precompute (`PrecomputeKeys`) is 4.3 to 4.5 s of those, before the first task
starts. The rest of the key, everything but the input digests, is about 2 s:
the option scope of each task (`JobConfigScope`) 0.7 s, the tree-fact
identities 0.9 s, the parameters and environment under 0.2 s.

The run looked up 3,343 input digests, 2,169 distinct, which selected 63,716
files in all. They split into 1,515 digests of ordinary patterns, 646 of
patterns that name an input selector, 7 of `git:` patterns (4 of the whole
cut, which select 52,496 of those files) and 1 of the empty pattern set. The
workspace walk they make reaches 13,232 files and 2,522 directories; a stat of
each of them takes 28 ms on this container.

A throwaway local build that answered each input digest from the previous
run's value, with no witness at all, bounds what this decision can save on
an unchanged tree (medians of three runs after a recording run):

| Measure | Main | Every digest answered | Selector digests collected |
|---|---|---|---|
| Wall | 30.7 s | 24.3 s | 26.2 s |
| Engine CPU | 42.7 s | 27.3 s | 32.3 s |
| Longest ready wait | 11.8 s | 9.3 s | 9.2 s |
| Key time summed | 20.0 s | 6.5 s | 9.9 s |
| Engine Git calls | 130 | 90 | 123 |

The third column keeps collecting the 646 selector digests and the empty
pattern set: they are 5 s of engine CPU, so the witness must cover a digest
whose files an extension listed, not only one whose names decide its
selection.

## Decision

### 1. A run records each input digest with its witness

A run that computes an input digest, the value `lookupFileHash` memoizes,
records it in `.putnami/input-record` with its witness: what the collection
read on disk to select and fold its files. The record is keyed by the memo key
of the digest (its directory, its patterns, its option scope, and the identity
of the expander when a pattern names an input selector) and holds the digest.
The next run answers the digest from the record when every part of its
witness holds, and collects it otherwise. A recorded digest is the digest the
collection gives: a cache key holds the same value as before, and no key,
entry address or protocol value moves.

The witness of a digest is:

- **Each selected file**: its absolute path and its stat, the stat of ADR 0009
  (device, inode, mode, owner and group, size, modification and status-change
  times to the nanosecond), taken by the collection as it selected the file. A
  file whose stat equals the recorded one has the digest its reading recorded,
  under the rule of ADR 0009. A file the key reads as unreadable is witnessed
  the same way.
- **Each directory the collection listed**: its absolute path and its stat,
  taken before and after the listing. This covers a `**` walk from a
  pattern's static prefix, the directories `filepath.Glob` lists for a
  pattern's glob segments, and the default walk of the empty pattern set. A
  directory's modification and status-change times move when an entry is
  created, removed or renamed in it (section 2), so an equal stat says the
  listing is unchanged; a change below it is seen through the directory that
  holds it, which the walk listed too. A directory the walk skips
  (`node_modules`, `.git`, `.putnami`, `out`, `dist`, `vendor`, dot-names for
  the default walk) is not listed and not witnessed: no file below it can be
  selected.
- **Each absent path the selection depends on**: a Git candidate the cut lists
  that is absent from the work tree, and a pattern's static prefix that does
  not exist, witnessed by the stat of the nearest existing directory above it.
- **For a `git:` pattern, the cut's witness** (section 3).
- **For an input selector, the answer's witness**: the answer of the extension
  is already kept across runs, keyed by its extension's implementation
  identity and validated by the digest of its basis
  (`internal/jobs/inputresolve`, `protocols/workspace` ADR 0004). The record
  witnesses that basis as it witnesses a pattern set: the files of its
  `content` sets by their stats, the directories its `content` and `listing`
  sets listed by theirs. The files the answer lists are then selected files.
  The expander's identity is in the memo key, so an extension that changed
  answers through another entry.

A digest gets no record, and is collected every run as today, when any part
of it has no witness: a pattern, a directory or a basis set that reaches a
file or a directory through a symbolic link (a link's target can change
outside every witnessed directory), a pattern that holds a character class or
a `.`, `..` or empty segment, an expander with no identity, or an answer whose
basis the extension did not declare. The record never widens the class of
digests on the extension's word: an answer with no basis is recollected.

A run answers a recorded digest at its first lookup only after checking the
whole witness, one stat per path. Stats are taken once per path per run and
dropped by the run's writes as ADR 0109 drops digests: a write drops the stats
at, under and above its paths, and a write the run cannot bound (a source
rewriter, which drops every digest) drops them all. A recorded digest that
holds enters the run's memo with the in-run witness of ADR 0109, rebuilt from
its recorded files, so a write of the run moves it as it moves a collected
one.

### 2. The racy rule, for files and for directories

Every part of a witness follows ADR 0009 §2, against the clock the run that
records reads: a file, a directory, the index or an ignore source is recorded
only when both of its times are older than that clock by more than two seconds,
and only when its stat before and after the read are equal. A digest with one
part that fails is not recorded, and the next run collects it again. A run
whose clock went back while it ran records nothing, and a record whose stamp
lies ahead of the clock discards itself.

A directory is trusted on the file systems ADR 0009 §3 lists, on the same
platforms, and on none other: on each of them a directory's modification and
status-change times move when an entry is created, removed, renamed in or out,
or replaced by a rename. Checked on 2026-11-26 on this container on ext4,
tmpfs and overlayfs (an ext4 lower layer, a tmpfs upper layer): creating,
removing and renaming a file, a directory and a symbolic link, and replacing
a file by a rename, moved the directory's stat on all three; appending to a
child, changing its mode and touching it did not, which the child's own stat
witnesses. On overlayfs a write in a lower-only directory copies it up and
moves its stat too. NFS, SMB, 9p and FUSE keep attribute caches another host
can make stale, and FAT keeps no status-change time: on them nothing is
recorded. Windows records nothing (decision `windows-support-is-suspended`, and ADR 0009 §3).

A file system that folds case keeps the rule: a rename that only changes the
case of a name moves its directory's stat, and the record compares paths as
the collection spelled them.

### 3. The cut's witness

A `git:` pattern selects candidates from the cut (`ls-files --cached --others
--exclude-standard`) and their index modes. Since ADR 0112,
the engine reads both itself, and falls back to Git for every state its reader
does not read. The witness depends on which of the two answered.

**In-process.** The reader returns, with its answer, what it read, and the
witness is exactly that:

- the index stamp (`git.ReadIndexStamp`: whether the index exists, its size,
  its modification time and the checksum Git writes at its end), taken before
  the read and equal after it. It is the same size and checksum by which the
  reader keeps a parsed index, and it moves whenever Git writes the index, a
  split index's shared file included;
- each directory the untracked walk entered, by its stat. That is every
  directory at or below the pattern's prefix that the ignore rules do not
  exclude, plus every directory the index tracks. An untracked file or
  directory can become a candidate only in such a directory, and creating it
  moves that directory's stat. An excluded directory is not entered: below
  it, nothing becomes a candidate while the ignore sources hold, since Git
  never re-includes a path below an excluded directory;
- each directory the walk listed without entering, by its stat: an untracked
  nested repository, listed as its directory. Creating or removing its `.git`
  moves its stat and would change whether the walk enters it. A gitlink is
  witnessed by its index entry, which the stamp covers;
- each ignore source the walk applied, by its stat, absent ones included
  (witnessed by the stat of the directory that would hold them): every
  `.gitignore` of an entered directory, `$GIT_DIR/info/exclude`, and the file
  `core.excludesFile` names, or the XDG default;
- the configuration reading of ADR 0112 §4, by the stats of the files it came
  from (`/etc/gitconfig`, the global and XDG files, the repository's `config`
  and `config.worktree`, absent ones included). A reading the reader takes
  again each time, because it also came from another file, from no file, or
  with a conditional include, gives no witness;
- the values of the environment variables that select those files or make
  the reader fall back: `HOME`, `XDG_CONFIG_HOME`, `GIT_CONFIG_GLOBAL`,
  `GIT_CONFIG_SYSTEM`, `GIT_CONFIG_NOSYSTEM`, and the variables of ADR 0112 §5
  (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
  `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`,
  `GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM`), recorded
  unset.

**Git fallback.** When the reader answers "unsupported" (ADR 0112 §5: a
workspace below the work tree's root, a submodule or a separate git dir, a
linked worktree's split index, a sparse index, `index.skipHash`, an
environment override, an ignore file it cannot read, and the others listed
there), a `git:` digest gets no record and is collected every run, as today.
Git reports neither the directories it entered nor the ignore files it read.
Rebuilding them beside it would be a second reading of Git's walk, which is
exactly the reading those states keep out of the engine. A recorded digest
whose witness was taken in-process, looked up in a run whose reader falls
back, is not answered: the run collects it through Git.

The witness says the listing is unchanged. It does not answer the run's own
read of the cut, which ADR 0112 §3 keeps reading afresh when a write of the
run asks it. Until ADR 0112's reader lands, no `git:` digest is recorded.

### 4. The key and how each of its inputs is witnessed

The record holds input digests, not keys. Every other input of the key is
computed by the run that uses it, as today, and a key is composed every run
from them, the recorded digests and the keys of its dependencies. The table
lists every field `CacheKey.ComputeHashUsing` folds (`store/cache.go`,
`cacheKeyVersion` `v9`):

| Key input | How the run has it |
|---|---|
| Key version, extension name, extension version | Constants and manifests the run loads |
| Extension implementation digest | Computed by the run: an installed extension's tree digest, kept per store entry (`implementationdigest`, ADR 0102); a local extension's runtime digest from the runtime sync, whose files the file index answers |
| Toolchain version | Computed by the run: the CLI's Go version and each toolchain's identity from the lock and the recorded probes (ADR 0102) |
| Runtime identity: host platform | Computed by the run |
| Runtime identity: release baseline | Read once per run (ADR 0110) |
| Runtime identity: tree facts (repository state, tracked files, ignored directories, candidate listings) | Read once per run from the run's Git reads (ADR 0002 and 0004 of `protocols/job`; in-process under ADR 0112) |
| OS class, source state, emptied outputs | Computed by the run |
| Task, task contract digest, project, project metadata digest | Computed by the run from the manifests and the workspace snapshot, which ADR 0009 validates |
| Embedded version | Computed by the run before any key (D-010) |
| Selected projects | The run's selection |
| Parameters | Merged by the run from the manifests, the configuration and the command line |
| Project files, workspace files, closure files | **Recorded digests**, under the witness of section 1 |
| Extra files (`options.generate.assets`) | Collected every run, as today: a full walk with no witness, a fraction of a millisecond on this repository |
| Environment | Read by the run from its environment |
| Dependency keys | Composed by the run, from the dependencies' own keys |

The option scope of a task (`JobConfigScope`, 0.7 s in the profile) and the
tree-fact identities (0.9 s) are computed once per run for each distinct
value instead of once per task: no witness, and the same values.

**A key input without a witness fails a test.** A table in
`internal/store` names each field of `CacheKey` with how the run has it:
`computed` with the reader that computes it, or `recorded` with its witness.
A test reflects over `CacheKey` and fails when a field has no row or a row
names no field; another fails when a collection path of `collectFilesWith`
(ordinary, default walk, `git:` cut, `git:` prefix, input selector) returns a
digest without a witness kind or without the explicit "not recorded" kind. A
new key input therefore lands with its row, and a new way to collect files
lands with its witness or recollects every run. The golden key tests of
`internal/jobs/cachekey` stay as they are: no key moves.

**A false hit fails a test.** A randomized test runs rounds of writes on a
Git fixture between two lookups of each digest kind: create, remove and
rename files, directories and links; rewrite a file with its size kept and
its modification time set back (`utimes`); change a mode; edit, create and
remove a `.gitignore`, `info/exclude` and the configured excludes file; stage,
unstage and remove paths; switch a branch; and land some writes inside the
racy margin. After each round it compares the digest the record answers with a
collection made with no record and no memo. A difference is a false hit and
fails the test. A second test checks each directory witness on the
file systems the machine offers (tmpfs, overlayfs where it can mount one).

### 5. Where the record lives and how runs share it

The record is `.putnami/input-record` in each worktree, beside the file index:
the stats of a worktree's files are its own, so another worktree's entries
never match (ADR 0009, "One index per machine"). It shares the file index's
envelope and rules:

- **Program identity.** The file names the program that wrote it, as ADR 0009
  §4: a record scheme version compiled into the CLI, the CLI's build
  information and the Go build ID of its executable. A record another build
  wrote loads empty; so does one the file index's reading-scheme version
  does not match, since a recorded digest folds the file index's readings.
  A process with no usable build ID records nothing.
- **Integrity.** The file ends with the SHA-256 of what precedes it; a file
  that does not parse or fails its checksum loads empty.
- **Concurrent runs.** Each run loads the record at its start and, at its end,
  merges into the file on disk under an exclusive lock and renames a complete
  file into place. An entry is decided when it is recorded, so a merge cannot
  make a racy entry trusted. Two runs that recorded one key keep the entry
  with the newer clock. Entries this run found out of date are dropped, and so
  is an entry no saving run recorded or confirmed in its last 32. The record
  holds at most 16,384 entries (2,169 on this repository) and 1,048,576
  witnessed paths. A run that only confirms entries writes nothing until they
  come halfway to being dropped, as the file index does.
- **What writes nothing.** `--plan`, a preview-only `--dry-run`, and a process
  that cannot read the clock read the record and write nothing.

A damaged or missing record costs a collection, never a stale digest.

### 6. The interface the machine process reuses

The machine process, one per machine and user and shared by its worktrees, will hold each
worktree's record in memory between runs. This record is what it holds, and
the witness is the contract it answers to:

- A witness is a list of parts, each a path and its stat, or a stamp (the
  index, an environment value). A digest's answer is "holds" when every part
  holds.
- A part holds when the path's stat equals the recorded one. A process that
  watches the tree may answer that a part holds without a stat only while it
  has watched that path, or the directory that holds it, without a break since
  it last took its stat, on a file system section 2 trusts. A dropped event, a
  queue overflow, a restart, a path it does not watch, a rename of a watched
  directory or a file system it cannot watch makes it take the stat.
- The answer is the same with and without the process: a run with no machine
  process, in CI or on a cold machine, takes every stat and gets the same
  digests and keys. The machine process accelerates and never decides
  correctness.

The design of the machine process itself is its own record.

## Consequences

Expected on this repository's unchanged run, from the bound above and the
witness's cost (a stat of every witnessed path, 28 ms for the whole tree,
and a record of about 2.8 MB, 15,755 distinct paths with their stats and the
references of 2,169 entries, which a synthetic record of that shape decodes in
4 ms): engine CPU from 42.7 s
to about 27.5 s, wall from 30.7 s to about 24.5 s, key time summed from 20.0 s
to about 6.5 s, engine Git calls from 130 to about 90, and the prune
precompute from 4.3 s to about 1.8 s of engine CPU. The implementation
measures them again on the same protocol.

- The longest ready wait goes from 11.8 s to about 9.3 s, not under 5 s. On
  an unchanged run it is set by how fast four workers drain 2,239 cache hits,
  and with the digests free each hit still pays its restore (3.7 s of engine
  CPU), its session record sanitizing (3.5 s), the remote negotiation (3.0 s)
  and the collector's work on them (2.9 s): the per-task fixed costs of
  an unchanged run, which are separate work.
- The first run in a new worktree, and the first after a CLI build changes,
  collects every digest: the record loads empty.
- A digest whose witness touches a path written less than two seconds before
  the recording run is collected by the next run, and recorded by it.
- A file written outside every declared output during a run is still not
  seen by that run until a write reaches it (ADR 0109); the next run checks
  every witness on disk and sees it.
- The risks ADR 0009 names for a file (a write through a memory mapping, a
  `lazytime` mount after a crash, a process that sets the clock back) apply to
  a directory too.
- Windows, and every file system outside section 2's list, collect every
  digest every run, as today.

## Rejected alternatives

- **Record each task's key, as the issue sketched.** With every input digest
  answered, the rest of the key costs about 2 s of engine CPU (measured above),
  and 1.6 s of it goes with a per-run memo of the option scope and the tree
  facts. A recorded key would also need a witness for every other input:
  parameters, environment, manifests, toolchain probes, tree facts, the
  release baseline. Each is a way to serve a stale key on every machine that
  shares the store, for well under a second. An input digest is shared by the
  tasks that read the same files (3,343 lookups for 2,169 digests), so a
  changed file moves one record entry, not the record of each task.
- **Git's untracked cache (`core.untrackedCache`).** It keeps directory stats
  for untracked files the same way, but only for `git:` patterns, only once a
  repository's configuration enables it, and it writes `.git/index` through a
  Git process, which `ls-files` does not do.
- **A watcher alone, with no witness.** Dropped events and a cold machine
  would serve stale digests; the epic's rule keeps the witness the decider.
- **One record per machine, in the store.** Every worktree has its own file
  times, so entries would not be shared, and every run of every worktree would
  contend for one lock (ADR 0009).
- **Directory modification time and size alone.** A tool that sets times back
  (`touch -r`, `rsync -t`) would keep a moved listing's stat; the inode and the
  status-change time cannot be set back, as for a file.
- **Record the selector digests by their answer alone.** The answer's basis is
  what the extension read to list the files; trusting the list without its
  basis's witness would keep an answer a new source file should have moved.
