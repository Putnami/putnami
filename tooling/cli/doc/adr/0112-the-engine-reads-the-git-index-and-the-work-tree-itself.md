# ADR 0112 — The engine reads the Git index and the work tree itself

- **Status**: accepted
- **Date**: 2026-11-26
- **Scope**: `@putnami/cli` (`internal/git`, whose `inprocess_*.go` files
  hold the reader, `internal/store`, `internal/engine`, `internal/cli`,
  `internal/workspace`, `internal/workspace_state`: the candidate cut, the
  source binding snapshot, the bounded `git:` listings, the index modes, the
  work tree root, HEAD, the branch, the clone's depth, the remote URL) and the
  SDK (`linetag`: the shared tag reads; `gitcandidate`: the job-side listing
  runs in the key's environment)

## Context

The engine asks Git about the local state of the repository many times per
run. Measured on 2026-11-26, on a 4-core Linux container, on an unchanged
`lint,test,build,validate --all --enforce-coverage` of this repository (every
task cached or skipped, 65 uncached job processes), the engine made 138 git
calls, 3.7 s of git CPU:

| Calls | Command | What it answers | Who asks |
|---|---|---|---|
| 39 | `ls-files -z -t --stage --others --exclude-standard` | The candidate cut, read again after each write of the run | `TreeFacts` (`CandidateFiles`) |
| 17 + 2 | `rev-parse --show-toplevel`, `ls-files --cached --others --exclude-standard [-- <prefixes>]`, `ls-files --stage [-- <prefixes>]` | A `git:` key that the run's cut does not serve: its root, its candidates and their index modes | `store` (`collectGitFilesWith`) |
| 15 + 15 | `ls-files --stage -z`, `ls-files --others --exclude-standard -z` | The source binding snapshot, read again after each write | scheduler (`ReadSourceBindingSnapshot`) |
| 8 | `rev-parse` (HEAD, `--short`, `--is-shallow-repository`, `--git-common-dir`, `HEAD^{commit}`, the repository state) | HEAD, the clone's depth, the repository state | version, impact, release baseline |
| 2 | `status` | Whether the tree is dirty, and the tracked changes | version, run marker |
| 1 each | `ls-files --ignored --directory`, `check-ignore`, `config --get remote.origin.url`, `show -s --format=%ct`, `symbolic-ref` | Ignored directories, whether `.putnami` is excluded, the remote, the commit time, the branch | tree facts, provenance, version |

Every listing of the first three rows reads the same two things: the index
and the untracked files the ignore rules do not exclude. Each call is a
process start that reads the whole index and walks the work tree.

## Decision

1. **The engine reads the index itself.** The reader, in `internal/git`
   next to the Git calls it replaces, reads
   `.git/index` as Git 2.43 reads it: versions 2, 3 and 4 (prefix-compressed
   names), the extended flags (skip-worktree, intent-to-add), the optional
   extensions it skips as Git does (`TREE`, `REUC`, `UNTR`, `FSMN`, `EOIE`,
   `IEOT`), and a split index (`link`, its two EWAH bitmaps merged into the
   shared index as `merge_base_index` does). It verifies the checksum. SHA-1
   and SHA-256 repositories are read. A process keeps a parsed index while the
   file has the same size and ends with the same checksum, which Git writes
   anew on every change of the index.
2. **The engine lists the untracked files itself.** It walks the work tree as
   `read_directory` does for `ls-files --others --exclude-standard`: the
   ignore rules of each `.gitignore` (BOM, CRLF, comments, escapes, trailing
   spaces, negation, anchoring, directory rules, `**`, and Git's `wildmatch`
   ported line by line), `info/exclude` and `core.excludesFile` (or the XDG
   default), in Git's precedence; an excluded directory is not entered; a
   directory the index tracks is entered; a gitlink is not; an untracked
   nested repository is listed as its directory; `.git` is skipped; regular
   files and links are listed, other types are not; `core.ignoreCase` folds
   ASCII case as Git does. With literal prefixes, it enters only the
   directories at, below or above one of them: what Git lists under a literal
   pathspec.
3. **One answer serves each former listing.** `CandidateFiles` (the cut),
   `ReadSourceBindingSnapshot`, `CandidatePaths`, `ReadIndexModes`, the
   tracked paths at or below a directory (`TrackedPaths`, which leaves a
   directory below the root under `core.ignoreCase` to Git) and the
   work tree root of a `git:` key are read in-process. Each reads afresh when
   its caller asked Git afresh before, so the run's invalidation rules (ADR
   0106, ADR 0109) are unchanged: this change replaces the reader, not when it
   reads.
4. **Git's configuration is Git's reading.** The values the reader needs
   (`core.ignoreCase`, `core.precomposeUnicode`, `core.excludesFile`,
   `core.bare`, `core.worktree`, `index.skipHash`, and the repository format:
   `core.repositoryFormatVersion` and `extensions.*` of the repository's own
   config file) come from one `git config -z --list --show-origin` in the
   root, with the environment of the calls it replaces
   (`gitread.RepositoryEnvironment`), so includes, conditional includes and
   every scope are Git's. The process keeps that reading while none of the
   files it depends on changes (size, modification time, inode): the files
   Git reads configuration from (`/etc/gitconfig`, the global and XDG files,
   the repository's `config` and `config.worktree`), every file an entry came
   from, every file an include or a conditional include names, present or
   not, and HEAD, which a condition on the branch reads. The files known
   before the read are stamped before and after it, and a file modified less
   than a second before the read began is too close to tell: a rewrite of the
   same size in the same tick of the clock keeps its stamp. A reading with a
   value from no file, an include whose path it cannot place, a change during
   the read or such a recent file is read again each time. A
   system configuration Git reads from another place than `/etc/gitconfig` is
   watched once it exists; one created there while a process runs is not.
   A reading is kept for the HOME and XDG_CONFIG_HOME it was made with.
   Every Git call the reader stands for runs with that same environment,
   fallbacks included: the bounded `git:` listing and the work tree root
   asked Git in the process environment before, so under `GIT_CONFIG_*` or
   `GIT_INDEX_FILE` they could list what the cut, read without them, did not.
   They now answer as the cut does, whether the reader or Git answers, and so
   does a job's own listing (`gitcandidate`), which a task and its key share. Under
   `GIT_LITERAL_PATHSPECS`, `GIT_GLOB_PATHSPECS`, `GIT_NOGLOB_PATHSPECS` or
   `GIT_ICASE_PATHSPECS`, which change how Git reads the literal pathspec of a
   bounded listing, the reader steps aside and Git's answer stands.
5. **Every state the reader does not read falls back to Git.** The reader
   answers "unsupported", and the caller runs the Git call it ran before, for:
   a root that is not the work tree's root (a workspace below it); a `.git`
   file that names no linked worktree (a submodule, a separate git dir), or
   one above 1 MiB; a
   linked worktree's split index; a repository owned by another user
   (`safe.directory`), a bare repository, a `core.worktree`; a repository
   format Git refuses (a version above 1, an unknown extension in version 1,
   a version 1 extension in version 0, an unknown extension in version 0,
   which a later Git may refuse, a repository config Git read no entry of);
   a rule whose meaning Git changed after 2.43 (a path rule whose literal
   prefix does not end with a slash and is followed by `**`, such as
   `foo**/bar`, which Git 2.52 no longer lets match `foobar`); any of `GIT_DIR`,
   `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`,
   `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`,
   `GIT_CEILING_DIRECTORIES`, `GIT_DISCOVERY_ACROSS_FILESYSTEM` or the four
   pathspec variables set; any repository on a platform that is not Unix,
   where the reader does not read ownership and file stamps; an index
   version other than 2 to 4, an unknown required extension, unknown entry
   flags, a sparse index (`sdir`: its directory entries need tree objects), an
   index without a checksum (`index.skipHash`) or with a wrong one, a corrupt
   entry or bitmap; a per-directory `.gitignore` that is not a regular file, a
   skip-worktree `.gitignore` absent from the work tree (Git reads it from the
   object store), a rule file it cannot read; a `.gitignore` spelled in
   another case under `core.ignoreCase`; a non-ASCII name under
   `core.precomposeUnicode`; a `.git` entry in the work tree that is neither a
   directory nor a file; a configuration value it cannot parse; an
   `extensions.objectFormat` other than SHA-1 or SHA-256; a work tree root
   across a file system boundary or a directory inside a git directory; any
   failure to read a directory. Outside every repository Git answers too, so
   the errors callers rely on (`*gitread.UnmanagedError`) are Git's.
6. **HEAD, the branch, the clone's depth and a remote's URL are read
   in-process too.** `HeadSHA` reads HEAD and the ref it names, loose or in
   `packed-refs`; `CurrentBranch` answers `symbolic-ref --short HEAD`, or
   HEAD on a detached HEAD; `IsShallow` reads the shallow file; `RemoteURL`
   takes the last `remote.<name>.url` of the configuration reading. The
   engine's run report, run marker, ancestry, commit check, automatic
   selection and session record call them. They step aside for a loose ref
   that is a link or a symbolic ref, a branch that is itself symbolic, a
   reftable, an unborn branch, a HEAD on another ref than a branch, a branch
   name Git's ref name rules refuse, a branch name a file of the git
   directory, `refs/<name>` (in the common or the worktree's own git
   directory) or `refs/tags/<name>` makes ambiguous, a detached HEAD a ref
   named HEAD makes ambiguous, a `packed-refs` file Git would refuse, and a
   shallow file that is empty or not a list of object names.
7. **Git keeps what only it answers correctly.** Reads of objects and
   history (the commit time, `HEAD^{commit}`, tags and their peeled commits,
   the tags HEAD reaches, the commits since a release), `status` (it
   refreshes stat data and compares contents), `check-ignore` and `ls-files
   --ignored --directory` (one call each per run), abbreviated object names,
   and the configuration read above stay with Git.
8. **The lines of one repository share their tag reads.** The release
   baseline of a tagged line asked git four times per line pattern: HEAD's
   commit, the tag names, the objects they peel to and the tags HEAD
   reaches (`linetag.LastRelease`). Each answer is the same for every line, so
   the run's release baseline reader and the version Head each hold one
   `linetag.Tags` that asks each once and answers every line from it (ADR 0110 reads each line's last release once; this
   reads what the lines share once). Measured on a full clone of this
   repository, with the engine's own calls traced on an unchanged run, 32
   line patterns made 128 of these calls at `29082f04` (32 of each), and 8 at
   `8fd15513`, one set for the release baseline reader and one for the
   version Head.
   Reading the tag names in-process (packed and loose refs) was not done: once
   shared, it saves one call per run, and the peeling and the reachability
   need objects. The per-project reads of a tagged line (the tag's commit,
   the object at the project, the commits since) stay with Git.
9. **A differential test holds the answers equal.** `internal/git` compares
   every in-process answer with Git's, over the shapes the candidate cut
   covers (ignore rules of every source, deleted, intent-to-add, renamed and
   unmerged paths, modes and links, gitlinks, nested repositories and nested
   `.git` files, sparse checkout, newline and non-UTF-8 names, index versions
   2 to 4, the untracked cache, a split index with additions, deletions and
   replacements, `core.ignoreCase`, a linked worktree, SHA-256, includes that
   appear or change, a NUL in a rule, loose and packed branches, a detached
   HEAD, a shallow clone), asserts for the unsupported states it
   builds (a skip-worktree `.gitignore` absent from the work tree, a sparse
   index, a root below the work tree, `index.skipHash`, a corrupt index,
   outside a repository, the refused repository formats, a bare repository)
   that the reader steps aside and the caller still gives Git's answer, and
   compares the cut of this repository. The other fallbacks are guards read
   from Git's sources, not built by the test.

## Rejected alternatives

- **Read the configuration in-process too.** Git reads it from up to five
  files with includes and conditional includes whose conditions read the
  branch. Measured on the 4-core container, one `git config -z --list
  --show-origin` in this repository (at `835c4d37`, Git 2.43.0) costs 1.8 ms
  of wall time and 1.5 ms of
  CPU, and the kept reading makes it once per process while the files do not
  change: a second reader of that grammar and its precedence would save less
  than 2 ms per run.
- **Expand a sparse index.** Its directory entries name tree objects, which
  needs the object store (loose and packed objects with deltas): a sparse
  index is rare, and Git reads it in one call.
- **Keep one walk for the run and patch it with the run's writes.** The
  listings read afresh because a write of the run can add, remove or ignore
  any path; reusing a walk across writes would make the reader decide
  correctness from file events, which ADR 0106 and the epic's rules keep out
  of the reader.
- **Use a Git library.** A library brings its own reading of the ignore rules
  and of the index, which is not Git's on several of the shapes above, and a
  dependency the CLI does not have.

## Consequences

- Measured on 2026-11-27 on the same container, on an unchanged
  `lint,test,build,validate --all --enforce-coverage` of this repository
  (`run.orchestration`):

  | Clone | Commits | Engine git calls | Git CPU | Engine CPU | Run |
  |---|---|---|---|---|---|
  | Shallow | `ef6c1aa2` → `8fd15513` | 138 → 17 | 3.7 → 0.5 s | 39.0 → 41.9 s | 61 → 59 s |
  | Full, with tags | `29082f04` → `8fd15513` | 724 → 508 | 6.5 → 3.5 s | 40.9 → 40.4 s | 50 → 45 s |

  On the full clone, 482 of the 508 calls are the per-project reads of a
  tagged line's release baseline (the tag's commit, the object at the
  project, the commits since), which stay with Git (item 7).
- The orchestration counts tests hold the lower engine counts.
- A change of Git's on-disk formats that this reader does not know (a new
  index version or required extension) falls back to Git; a change of the
  meaning of a known format would not, and the differential test is the guard
  that runs against the Git of each machine that runs the suite.
