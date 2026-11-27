# ADR 0113 — An input digest whose witness holds is not collected again

- **Status**: accepted (2026-11-27)
- **Date**: 2026-11-26
- **Amends**: [ADR 0109](0109-a-write-drops-a-memoized-digest-only-when-it-can-move-it.md)
  (its rejected alternative "Keep the digests across runs" becomes this
  decision), and extends the rule of `protocols/workspace`
  [ADR 0009](../../../../protocols/workspace/doc/adr/0009-a-files-stat-decides-that-it-is-unchanged.md)
  from a file to a directory
- **Depends on**: [ADR 0112](0112-the-engine-reads-the-git-index-and-the-work-tree-itself.md),
  whose in-process reader gives the cut's witness (section 3)
- **Scope**: `@putnami/cli` (`internal/store`: the input digests of cache keys;
  `internal/fileindex`: the record's envelope; `internal/jobs/cachekey`: the
  key inputs, the prune precompute and the release baselines of the tree
  facts; `internal/git`: the cut's witness)

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

The container's clone had no tags. On a full clone with this repository's 36
tags, measured the same way the same evening (medians of three unchanged runs
after a priming run), the release baselines that tasks declaring the
`releaseBaseline` runtime input key on add their own Git reads: 736 engine Git
calls instead of 130, of which 166 `log`, 150 `ls-tree`, about 180 `rev-parse`,
64 `for-each-ref` and 32 `cat-file`, and 4.9 s of Git CPU. Each of the 150
baselines a run reads asks Git for its tag's commit, the object that commit
holds at the project, and the commits since the tag that touch the project.
The same throwaway build, answering also each baseline from the previous run
with no witness:

| Measure, full clone | Main | Baselines answered | Baselines and digests answered |
|---|---|---|---|
| Wall | 34.7 s | 31.2 s | 26.9 s |
| Engine CPU | 44.5 s | 41.2 s | 29.3 s |
| Longest ready wait | 12.5 s | 11.1 s | 10.2 s |
| Engine Git calls | 736 | 227 | 189 |
| Git CPU | 4.9 s | 3.5 s | 3.5 s |

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
  times to the nanosecond), taken once the digest is computed (section 2). A
  file whose stat equals the recorded one has the digest its reading recorded,
  under the rule of ADR 0009. A file the key reads as unreadable is witnessed
  the same way.
- **Each directory the collection listed**: its absolute path and its stat.
  This covers a `**` walk from a pattern's static prefix, the directory
  `filepath.Glob` lists for a pattern's last segment, and each path a pattern
  without a glob looks up. The collection names what it read: a second
  reading of the glob (`listedGlob`) must select what `filepath.Glob`
  selected, or nothing is recorded. A pattern with a `..` segment or a
  character class is recorded that way. A
  directory's modification and status-change times move when an entry is
  created, removed or renamed in it (section 2), so an equal stat says the
  listing is unchanged; a change below it is seen through the directory that
  holds it, which the walk listed too. A directory the walk skips
  (`node_modules`, `.git`, `.putnami`, `out`, `dist`, `vendor`) is not listed
  and not witnessed: no file below it can be selected. Neither is a directory
  an exclusion excludes as a whole (`!**/.gen/**`): nothing it lists can be
  selected by the ordinary patterns. That pruning applies to the ordinary
  listing alone, never to the cut's witness or to an answer's basis, which
  read what they read whatever the patterns exclude.
- **Each absent path the selection depends on**: a Git candidate the cut lists
  that is absent from the work tree, a pattern's static prefix or a literal
  path that does not exist. The collection found it absent and the record
  finds it absent again; it is witnessed by staying absent, and holds while
  nothing exists at the path. Any other path a witness names that does not
  exist when the record takes its stat, such as a configuration file of the
  cut's witness or a directory a walk listed and that is gone since, is
  witnessed by the stat of the nearest existing directory above it.
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
of it has no witness: a selected file or a listed path that is itself a
symbolic link, dangling or not (its target can change outside every
witnessed directory); a pattern with a glob before its last segment, which
`filepath.Glob` follows through links; the empty pattern set; an expander
with no identity, or an answer whose basis the extension did not declare. A
link on the way to a witnessed path, a system one above the workspace or a
linked project directory included, does not prevent the record: each link
the resolution of a witnessed directory meets is witnessed by its own lstat,
which pointing it elsewhere moves, and every path below it is stated through
it. A link's lstat is trusted on any file system section 2 lists, outside
the margin, even when it is not the record's device, such as macOS's system
links on the System volume. Three paths are read by a link's text rather than through it, and a link
there is its own witness: a Git candidate of a digest of Git candidates
alone, as Git lists it; a path an answer's basis content set selects, as the
basis digest reads it; and a link the nearest existing directory above a
missing path is reached through, with the directory it leads to. The record never widens the class of
digests on the extension's word: an answer with no basis is recollected.

A run answers a recorded digest at its first lookup only after checking the
whole witness, one stat per path. Stats are taken once per path per run,
keyed by the exact path, and dropped by the run's writes as ADR 0109 drops
digests: a write drops the stats at, under and above its paths in any
spelling of their case, and a write the run cannot bound (a source rewriter,
which drops every digest) drops them all. A stat taken while a write was
dropped answers its lookup but is not kept. The parts of a digest whose
witness has a link are stated again at every check and never kept: a write
the run names by the physical path does not reach a stat kept under the
linked spelling. Such a digest costs a stat of each of its parts at each
check rather than once per run: on macOS every digest of Git candidates,
whose cut witness names `/etc/gitconfig` through the `/etc` link, and every
digest of a workspace reached through a link. A recorded digest that
holds enters the run's memo with the in-run witness of ADR 0109, rebuilt from
its recorded files, so a write of the run moves it as it moves a collected
one.

**A sample of the answers is collected again.** Each run collects again a
random sample of the digests the record answers: each answer with a
probability of 16 over the entries the record loaded, and at least 1 %, so
about 25 of this repository's 2,480 answers, and every answer of a record of
16 entries or fewer. A sampled answer that differs from its collection, or
whose collection now fails, shows a witness the record should not have
trusted, in a class no test foresaw, unless a write of the run came between
the answer and the collection, in which case the run uses the collection
and drops that entry alone: the run uses the collection (or returns its
error), discards the whole record and every digest the run memoized, so it
collects every digest again for the rest of the run, saves an empty record,
and reports one warning (`cache.input_record_discarded`) that names the
digest, its directory and its patterns, when it happens, so the run's output
and its result carry it. Keys the run computed before the discard are not
computed again. An unknown class of selection or listing witness that fails
becomes a miss within the run that samples it, and every run after it starts
from an empty record. The sample does not check the file witness: its
collection reads file contents through the file index, which answers by the
same stat (ADR 0009). The sample costs about 25 collections a run, 1 % of the
collections the record saves on this repository, and about 16 a run on a
smaller one (an answer looked up again after a write is sampled again). The tests that assert no false hit turn the sample off, so a
false hit fails them instead of being corrected.

### 2. The racy rule, for files and for directories

Every part of a witness follows ADR 0009 §2, against the clock the run that
records reads when it opens the record: a file, a directory, the index or an
ignore source is recorded only when both of its times are older than that
clock by more than two seconds. Each stat is taken once, after the digest is
computed. The collection read the path after the clock was read, so a change
to it after the read, and any change within two seconds before the clock,
leaves its times inside the margin, and the digest is not recorded; a stat
taken before and after the read would add nothing to that. A digest with one
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

**In-process.** The reader's cut (`localCut` in `internal/git`) returns, with
its answer, what it read, and the witness is built from it alone:

- the index stamp (`git.ReadIndexStamp`: whether the index exists, its size,
  its modification time and the checksum Git writes at its end), taken before
  the read and equal after it. It is the same size and checksum by which the
  reader keeps a parsed index, and it moves whenever Git writes the index, a
  split index's shared file included. A gitlink is an index entry, witnessed
  by the stamp;
- each directory of `Directories`, by its stat: the directories the untracked
  walk read, at, below or above the pattern's prefix. An untracked file or
  directory can become a candidate only in such a directory, and creating it
  moves that directory's stat. A directory excluded as a whole is not read:
  below it, nothing becomes a candidate while the ignore sources hold, since
  Git never re-includes a path below an excluded directory;
- each untracked nested repository the answer lists (`Untracked` with
  `Repository` set), by the stat of its directory: the walk lists it without
  reading it, and creating or removing its `.git` moves that stat and would
  change whether the walk reads it;
- each file of `RuleFiles`, by its stat: `info/exclude` and the global ignore
  file when they exist, and the `.gitignore` of each directory the walk read.
  A `.gitignore` a read directory lacks is witnessed by that directory's
  stat, which its creation moves. An `info/exclude` or a global ignore file
  that does not exist is witnessed by the stat of the nearest existing
  directory above its path; which path the global file has is witnessed by
  the configuration below;
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
run asks it.

### 4. A release baseline is recorded by the commits it names

A task that declares the `releaseBaseline` runtime input keys on its
project's release baseline (ADR 0110): the repository's state, the line's tag
pattern, the line's last release tag, the object that tag's commit holds at
the project's prefix, and whether a commit since the tag that touches the
project declares a breaking change. The run records each baseline it reads,
keyed by the project's directory and the tag pattern, with its tag's commit
and the project's prefix.

Every part of a baseline but the repository's state is a function of commit
ids, the prefix and the pattern: Git's objects are named by their content, so
the object a commit holds at a path, and the commits between two commits that
touch it, never change. The witness is therefore what names those commits,
read through the shared tag reads of ADR 0112 §8 and the repository state of
ADR 0110:

- the commit HEAD names;
- every tag name with the object it peels to (a fingerprint of the whole
  list, not only of the pattern's tags: an untagged line keys on whether the
  repository has any tag);
- the tags HEAD reaches, each with the object its ref names (the
  `for-each-ref --merged` read the last release is decided from);
- the repository's state: inside a work tree, not shallow, and the root's
  prefix, from which the project's prefix follows;
- that nothing rewrites history as Git reads it: no replace ref (no
  `refs/replace` directory and no `refs/replace/` line in `packed-refs`, read
  in-process), no `info/grafts` file, and none of `GIT_REPLACE_REF_BASE`,
  `GIT_NO_REPLACE_OBJECTS`, `GIT_GRAFT_FILE` set;
- Git's whole configuration reading (the SHA-256 of `git config -z --list
  --show-origin`, as the in-process reader keeps it under ADR 0112's stamps)
  and the values of every `GIT_` variable, `HOME` and `XDG_CONFIG_HOME`: a
  commit encoding setting changes the bytes of a commit's message that the
  breaking-change check parses, and this reading covers includes,
  conditional includes and command-line configuration, which the file stats
  of section 3 do not.

A baseline whose witness holds is answered with no Git call of its own: no
`rev-parse` of its tag, no `ls-tree`, no `log`. One whose witness moved is read
again, as today. A baseline a run read is written only when the whole
witness, taken again from reads of its own when the run saves the record, is
unchanged: a tag, HEAD or the configuration that moved while the baselines
were read leaves them out. What remains: a configuration, replace-ref or
grafts change that comes and goes between the witness and the per-project
`log` read. A shallow repository, a repository
outside every work tree and one with no commit read their state as today and
record nothing: their baselines take no per-project read. The breaking-change
check is the CLI's code, so another CLI build empties the record (section 6).

The files a job of such a task receives at the baseline
(`releaseBaselineFiles`) are extracted only for a task that runs; a cache hit
extracts nothing, and the extraction is unchanged.

### 5. The key and how each of its inputs is witnessed

The record holds input digests and release baselines, not keys. Every other input of the key is
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
| Runtime identity: release baseline | **Recorded baselines**, under the witness of section 4; read again, once per project and pattern (ADR 0110), when it moves |
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
file systems the machine offers to the test: its temporary directory, and
tmpfs at `/dev/shm` where it exists.

### 6. Where the record lives and how runs share it

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
  of the run that saves last. Entries this run found out of date are dropped, and so
  is an entry no saving run recorded or confirmed in its last 32. The record
  holds at most 16,384 entries (2,169 on this repository) and 1,048,576
  witnessed paths; a save that would exceed either keeps entries in key order
  up to it. A run that only confirms entries writes nothing until they
  come halfway to being dropped, as the file index does.
- **Release baselines.** The baselines landed first, in their own file,
  `.putnami/release-record` (`internal/fileindex/release_record.go`), with
  the same envelope rules and their own lock and 16,384-entry cap. The slice
  after this one moves them into the input record as its table
  `release-baseline` and deletes that file's envelope.
- **What writes nothing.** `--plan`, a preview-only `--dry-run`, and a process
  that cannot read the clock read the record and write nothing.

A damaged or missing record costs a collection, never a stale digest.

### 7. The interface the machine process reuses

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

### 8. What prune can settle with the record

On an unchanged `--all`, prune keeps the 48 `test~test-env` and
`test~test-env-teardown` tasks: a test's precomputed key can still be moved by
a planned writer that is not done when `test-env` starts (the same project's
`config-merge`, `describe`, `generate` and `infra` steps,
`validate-workspace~codeowners`, and with `--fix` each `lint~golangci-lint`,
whose rewritten region holds `go.work` and `go.work.sum`), and
`SettlesBefore` accepts only a writer that finishes before the producer or
runs after the consumer.

The record does not change what a writer may reach, so it settles none of
them by itself. It makes a third answer cheap and sound: **a writer that
writes nothing cannot move a key.** A writer served from its entry writes only
what differs from the files on disk (ADR 0108), so it writes nothing when its
precomputed key has an entry and every output that entry holds equals the file
on disk, whose digest the file index answers by stat. Prune may then treat a
writer as settled when that holds and its own key is not moved by a writer
outside that set, a fixpoint over the plan. Precompute then costs about 1.8 s
instead of 4.3 s (section "Context"), and the check of the outputs costs one
stat per output.

The fixpoint holds only as far as the writers have an entry prune can find
at their precomputed key. Traced on 2026-11-27 with the real CLI, on main and
at `ef6c1aa2`, on a shallow clone and on a full clone with its tags: of 2,194
cacheable jobs, none starts with a key other than its precomputed one, and
the only jobs served where the precompute found no entry are the 112
source-bound `describe` and `generate` steps (35 `build~describe`, 35
`test~describe`, 22 `build~generate`, 20 `test~generate`), whose entry
address binds the project's source binding. Binding-aware presence
(`cachekey.CacheEntryPresentAt`) finds them, at no Git call under ADR
0112. A count of 488 taken earlier came from a branch build whose identity
differed from the store's entries; its other classes do not occur on main.

Prune itself still counts a source-bound entry absent: its key movement
covers keys, not bindings. The fixpoint therefore asks
`CacheEntryPresentAt` and checks the entry's outputs on disk. The expected
gain, the 48 job processes, is measured when prune asks the fixpoint; this
record creates no entry and moves no key.

## Consequences

Measured on 2026-11-27 with the protocol above, on the same 4-core container,
on a full clone with its 36 tags, the input digests of sections 1 to 3 with
the sample of section 1, against main `13d56d86` (which already reads the Git
index in-process, ADR 0112, and records the release baselines, section 4),
medians of three unchanged runs after a priming run:

| Measure | Main | Input record |
|---|---|---|
| Wall | 22.2 s | 19.2 s |
| Engine CPU | 27.3 s | 22.5 s |
| Run CPU (`runMs`) | 30.1 s | 25.3 s |
| Key time summed | 7.2 s | 8.7 s |
| Longest ready wait | 4.8 s | 5.2 s |
| Input digests answered / collected | 0 / every one | 2,486 / 158 |

Engine Git calls stay at 58: the section 4 record already removed the
per-project release-baseline reads. The key time summed is the wall time
tasks spend in their key step, waits included, not CPU: it rises by 1.5 s
while the engine's CPU falls by 4.8 s and the wall by 3.0 s; where those
waits come from is not measured here. The digests still collected are those of output directories restores
rewrite on every run (a project's `.gen`, where a digest keys on files
there), the sample, and the ones a link or a basis with no answer identity
leaves unrecorded.

The witness costs a stat of every witnessed path, 28 ms for the whole tree
(a digest read through a link pays its stats at each check, section 1),
and the record is a few megabytes; a synthetic record of this repository's
shape (15,755 distinct paths with their stats, references of 2,169 entries)
decodes in 4 ms.

- The longest ready wait stays near 5 s (4.8 s to 5.2 s). On an unchanged
  run it is set by how fast four workers drain 2,245 cache hits, and with the
  digests free each hit still pays its restore, its session record and the
  collector's work on it: the per-task fixed costs of an unchanged run,
  which are separate work. The owner moved the < 5 s
  ready-wait criterion to the per-task fixed-cost work on 2026-11-27. This
  decision is done when no input digest is collected while its witness holds,
  the randomized write tests find no false hit, and the gain is measured on
  the protocol above.
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
  parameters, environment, manifests, toolchain probes, the other tree
  facts. Each is a way to serve a stale key on every machine that
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
