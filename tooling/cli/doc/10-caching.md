# Caching

The CLI provides transparent caching for job results and output artifacts. When a job is re-run with identical inputs, the cached result is restored instantly without re-executing the subprocess.

## Cache Architecture

```
~/.putnami/store/<repo-id>/      # machine-global, shared by every worktree of a repo
├── blobs/{hash[0:2]}/{hash}/    # Content-addressed cache entries (the Action Cache)
│   ├── meta.json                # Entry metadata
│   ├── result.json              # Cached job result
│   ├── manifest.json            # Per-file CAS digests for the entry's outputs
│   ├── files/                   # Output artifacts (hardlinks into cas/)
│   └── lastused                 # Last-hit timestamp (drives GC recency)
├── cas/{digest[0:2]}/{digest}   # Raw, deduplicated file bytes (content-addressed)
├── leases/{hash[0:2]}/{hash}.lease # Transient compute/restore ownership + heartbeat
├── tmp/                         # Staging area for atomic writes
└── .lock                        # Advisory lock: publishers/leases (shared) vs GC (exclusive)
```

The cache is content-addressed: each entry is identified by a SHA-256 hash of all inputs that affect the job outcome. Identical bytes across entries (and across worktrees) are stored once in `cas/` and hardlinked into each entry's `files/` tree.

### Store Location & Sharing

The store is **machine-global and per-repo**, not per-workspace. Agentic tools create many git worktrees of the same repo; keying the store by the repo's git common dir (which every worktree of a repo agrees on) lets all worktrees share one store. The first build in any worktree warms the cache for every sibling, and because remote-cache hits materialize into this local store, a remote blob is downloaded **once per machine** rather than once per worktree.

Resolution order for the store root:

1. `PUTNAMI_STORE_DIR` — used verbatim (inherited into nested `putnami` like other `PUTNAMI_*` vars, so a parent run and its children share one store).
2. `~/.putnami/store/<repo-id>` — `repo-id` hashes `git rev-parse --git-common-dir`; non-git checkouts fall back to a hash of the workspace root (still under the global location).
3. `<workspace>/.putnami/store` — last resort when `$HOME` is unavailable.

> **`cacheRoot` is not the store.** The mutable scratch directory handed to jobs/install hooks as `cacheRoot` (and `PUTNAMI_CACHE_ROOT` / the `{cacheRoot}` template var) is `<workspace>/.putnami/cache` — deliberately **per-workspace**. It holds arbitrary non-content-addressed scratch such as job-context temp files. Core leases protect it from reclamation, but do not serialize concurrent writers. A cache with its own cross-process safety contract may opt into a dedicated shared root; the Go compiler/module cache does so below.

### Workspace scratch retention

Scratch is disposable and belongs to a **30-day generation**. The next idle job
execution, install hook, or `putnami cache gc` in that workspace deletes the
whole expired generation. This includes orphaned slots left by deleted, moved,
or renamed projects, without core knowing an extension's directory layout.
An existing scratch root without a generation stamp gets its first 30-day
window when the updated CLI observes it. Expiry also clears still-useful
incremental state; the next use rebuilds it.

There is **no hard byte quota**. Its size is bounded by what installed
extensions write during one generation, plus any deferral while consumers are
active. For example, `go-describe/<project-path>/host-binary` holds one host
binary (about 20 MB) per described project path seen in that window: twelve
paths are about 240 MB, and renames can add slots until expiry. Temporary job
contexts and task-capture directories are normally removed by their owner;
generation expiry also recovers abandoned ones. Unused worktrees are not
scanned by other worktrees and retain their files until used again or removed.

The permanent `<workspace>/.putnami/cache.lock` and `cache-generation` stamp
live outside the reclaimed root. Runs hold a shared lease across all tasks and
output capture; subprocesses inherit a separate lease descriptor, so even a
detached child retains protection after its parent exits. Reaping takes an
exclusive lock without waiting for active consumers, including nested CLI
runs. A continuously running consumer postpones expiry until the root is idle.
Concurrent consumers must use this CLI's lease contract; old CLI versions and
direct writes outside Putnami do not participate.

Normal use does no recursive scan. An expiry pass uses confined, non-symlink-
following deletion with bounded directory buffers; total work scales with the
number of entries and has no hard time budget. Deletion errors warn and leave
the generation expired for retry; an unavailable consumer lock fails execution
closed. Reaping is disabled on platforms without cross-process file locks.
The shared CAS, machine-global extension caches, and per-project pre-build hook
caches outside this root keep their existing lifetimes. See
[ADR 0031](adr/0031-scratch-generations-expire-between-consumers.md).

## Cache Key Computation

A cache key is a SHA-256 hash composed of:

```
v8                          ← format version (for compatibility)
+ extension name            ← e.g., "@putnami/typescript"
+ extension version         ← only when the extension has no implementation digest (see below)
+ implementation digest     ← what the extension runs; empty when it has none
+ toolchain                 ← the CLI's Go runtime and each declared runtime toolchain's lock identity
+ runtime identity          ← declared runtime inputs such as hostPlatform, absent when none
+ OS class                  ← "windows" on Windows, absent elsewhere
+ source state              ← "unmanaged" where Git does not manage the workspace root, absent inside a repository
+ task name                 ← e.g., "build~transpile"
+ task contract digest      ← the task's declaration in the manifest
+ project name              ← e.g., "my-app"
+ project metadata digest   ← provider-owned project metadata
+ workspace version         ← from putnami.workspace.json
+ publish version           ← full version incl. commit suffix, only for version-bearing tasks (see Version-Aware Tasks)
+ task params hash          ← resolved task inputs plus owning-command flags
+ file content hash         ← SHA-256 of source files matching patterns
+ env vars hash             ← values of declared environment variables
+ upstream hashes           ← cache hashes from dependency tasks
```

### Extension Implementation

Every key names the implementation of the extension that runs the task, in
one of three ways:

| Extension | Key carries |
|-----------|-------------|
| Workspace-local, with a prepared runtime | The prepared-runtime digest (see [Artifact Store](#artifact-store-binaries)) |
| Installed: lock-pinned, per-worktree install, or `node_modules` package | The content digest of the installed tree, without version metadata |
| Workspace-local without a prepared runtime, or local outside the workspace | The extension version |

When the key carries a digest, it does not carry the version, so two builds of
an extension that differ only in the version they stamp share their entries.
Every framework build stamps a new version, so moving between canary builds of
an unchanged extension keeps the cache warm.

The installed-tree digest reads every regular file (path, executable bit and
bytes), every symbolic link (path and target), and every other file, such as a
named pipe (path and type), in the installed directory. It removes the version
an extension build stamps from the two files the extension protocol defines,
each re-encoded as canonical JSON:

- the top-level `version` of `putnami.extension.json`, and its
  `agentContent.manifestSha256`, which binds the agent-content manifest bytes
  that carry the version;
- the top-level `version` of the agent-content manifest
  (`<agentContent.path>/putnami.agent-artifact.json`).

A file among these that is not one JSON object is hashed as raw bytes. A
version in any other file, such as a package manifest, is the extension's own
content. Any other change to the tree, such as a runtime binary, a
configuration file or an executable bit, moves the digest and misses the cache.

The digest does not decide which files a task reads, so documentation counts
like any other file. Any byte of the installed archive moves the digest,
including `README.md`, `AI.md` and the framework docs the Go extension bundles
under `framework-docs/`. A build that changes only documentation misses the
cache for every task of the extension.

A runtime binary that embeds its version or its commit moves the digest at
every build. The Go and TypeScript extension runtimes are built without a
version variable and with `buildvcs: false`, and read their version from the
manifest at run time, so their bytes change only when their sources do.

Two key fields differ between platforms as a side effect of what they
identify:

- The implementation digest reads the binaries of the installed platform, so
  it differs on each platform for an extension that ships platform
  executables.
- The toolchain field differs on each platform for a task that uses a runtime
  toolchain: its own, or one the extension's runtime declares for every task,
  as the Go and TypeScript extensions do.

Only a task with neither shares its entries between a developer machine and
CI on another platform. A task whose output depends on the platform still
declares the `hostPlatform` runtime input, because neither field is that
declaration.

A runtime toolchain pin keys the task through the toolchain field, whatever
the implementation digest.

### Task Parameters

The primary cache inputs are parameters the task declares with an input port
such as `"target": {"from": "params"}`. The key uses the value the task
actually receives after workspace defaults, project options, and CLI flags are
merged. Kebab-case declarations and their supported camelCase spelling resolve
to the same canonical cache input.

Flags defined by the task's owning command are also included as a conservative
safety boundary for older or third-party task contracts that omit a consumed
parameter. This can make sibling steps of one command miss together, but it
prevents an unsafe hit when a valid flag such as `test --race` changes behavior.

Parameters carried only by a parent command do not invalidate its dependency
tasks. For example, `publish --channel latest` can reuse an otherwise-identical
build task because `channel` is neither a declared input nor a flag of the
`build` command, while changing its `target`, `minify`, or `sourcemap` input
still produces a miss. Operational parameters such as project selection,
output formatting, and parallelism likewise stay outside task keys unless the
task contract or its owning command defines them.

### File Content Hashing

File hashing algorithm:

1. **Collect files** — Match project input globs against the project directory and `from: "workspace"` input globs against the workspace root.
2. **Sort** — Sort file paths lexicographically for determinism.
3. **Deduplicate** — Remove duplicate paths.
4. **Hash** — For each file: write the relative path, then stream the file contents through SHA-256.

When no patterns are specified, all non-hidden, non-ignored files in the project are hashed (excluding `node_modules/`, `.putnami/`, `out/`, and dotfiles).

A selected file that cannot be read — a permission-restricted source, or a file
replaced mid-run by an editor's atomic save or a concurrent generator — still
contributes its relative path plus an `__unreadable__` marker instead of its
contents. Its key therefore differs both from the key for the same tree with the
file readable and from the key for a tree that does not contain the file at all,
so an entry built without the file is never served as a hit for a tree that has
it. A file that is intermittently unreadable produces a miss on the runs where
it cannot be read, which is the correct answer: those runs are not the same
input.

### A project config is hashed through the reading task's scope

`putnami.json` (and its `.putnamirc.json` spelling) is one file addressed to
several readers. A task that reads it as configuration hashes it semantically
rather than byte for byte. That task's key reads:

| Part of the file | In the key | Why |
|---|---|---|
| identity, `dependencies`, `disable`, everything outside `options` and `tasks` | yes | it decides what the task builds |
| `options.<cmd>`, and any namespace that is not spelled like an extension (`sdd`) | yes | the hasher cannot attribute it to one extension, and a dropped input serves a stale result |
| `options.<this ext>`, `options.<this ext>:<cmd>`, and the same by path reference | yes | this extension's own settings |
| `options.<other ext>`, `options.<other ext>:<cmd>` | **no** | that block is another extension's input; this task cannot read it |
| a bare namespace another manifest declares under `optionNamespaces` (`sdd`) | **no** | the manifest that reads it says so; one nobody declares, or two declare, stays |
| `tasks` | no | CPU weight and deadlines are execution-only |

Two kinds of task read the file as text instead. Their key holds every byte of
it, `tasks` and layout included:

- A task that rewrites its sources (see
  [Source-writing fix tasks](#source-writing-fix-tasks)), whatever pattern
  selects the config. A formatter can change only the layout of a config, and
  the semantic reading hides that edit.
- A task whose pattern selects the config with a glob in its last segment:
  `**/*.json`, `*.json`, `templates/app/**`. Such a pattern selects files by
  type or by directory, as a linter or a template copier does. A pattern that
  names the file (`putnami.json`, `**/putnami.json`, `../putnami.json`) keeps
  the semantic reading. When both kinds select one config, it is read as text.

The raw digest is tagged, so a config whose bytes equal the canonical JSON of
its semantic reading still keys differently under the two readings. A task
that declares no pattern hashes its whole project tree, and that walk keeps the
semantic reading.

An extension layer is recognized by spelling: a package name (`@putnami/cloud`)
or a workspace path reference (`/typescript/extension`), bare or with a
`:<command>` suffix. Anything else stays in the key. A hasher with no task in
hand — `cache verify`, any caller that does not know whose key it is building —
keeps the whole file, which is the wider of the two projections and never the
wrong one.

Measured on this repository: adding a `@putnami/cloud` deploy block to
`go/framework/http` re-ran 30 cacheable tasks across six projects before the
scope and 0 after it. Changing `options.sdd` cost the same 30 until
`@putnami/sdd` declared that namespace in its manifest
(`optionNamespaces`, see the [manifest reference](../../../protocols/extension/doc/01-manifest.md#option-namespaces));
a namespace no manifest declares still re-runs everything, because attributing
it by convention would be a guess.

Scoping is about a file's CONTENT. Adding or removing a project-config file
still moves the key, because the set of files an input selects is part of the
key by construction.

### A project's declared inputs are per command, never per task

`options.<command>.filePatterns` in a project manifest folds into the key of
**every** task of that command, so a document declared for one test is an input
of the whole suite. That is why the gates reading this repository's own
documents — the governance surface, the contributor recipe, the release plan and
its recorded verdict, the public-cut candidate — live in
[`tooling/cli-documents`](../../cli-documents/README.md) rather than in
`@putnami/cli`: editing `CONTRIBUTING.md` now re-runs that small suite instead of
the CLI's whole test task.

A pattern that reaches outside the project must be spelled the way
`filepath.Rel` produces it, because `collectFiles` matches the cleaned relative
path: a sibling under `tooling/` is `../sibling/**`, and
`../../tooling/sibling/**` selects nothing at all.
`TestDeclaredFilePatternsAreInTheFormTheCacheKeyMatches` fails on the second
spelling.

### Git candidate inputs

A `git:` pattern enumerates `git ls-files -z --cached --others
--exclude-standard` in the containing repository, then applies the same
project-relative matching and exclusions. `git:**` covers every candidate,
including files outside the project's directory. It never walks ignored local
directories. Missing tracked files are absent from the candidate; additions,
edits, renames and deletions change the key. Staging unchanged bytes does not.

Candidate inputs hash raw file bytes and symlink target text, with separate
file-type markers. They do not follow symlinks or normalize project-config
task tuning or version-stamp timestamps: a repository scanner can read those
bytes. When ordinary and Git patterns select the same file, the raw candidate
digest wins. Git enumeration errors and unsupported non-regular candidate
files prevent key computation. Ordinary patterns keep their existing behavior;
adding `git:**` does not filter files an ordinary pattern separately selects.

The document gates declare `git:**` instead of a fixed list of repository
documents, so the public-cut scanner and its cache key cover the same tree.
See [ADR 0041](adr/0041-git-candidate-file-inputs.md).

### Environment Variable Hashing

When a task declares `cache.key.env` (or uses `env` input ports), the values of those environment variables are included:

```
SHA-256(name1=value1\0 + name2=value2\0 + ...)
```

Variable names are sorted for determinism.

### Upstream Hash Propagation

When task B depends on task A (via `^` or explicit pipeline dependency), A's cache hash is included in B's key. This means:

- If A's source changes → A gets a new hash → B also gets a new hash → both re-execute.
- If A's source is unchanged → A has the same hash → B's key is unchanged → B hits cache.

The propagation is exactly as precise as A's declared inputs. A task that
carries a `^` edge is where an over-wide declaration stops being local: every
file in A's key is a file that re-keys B, and B's own dependents after it. Go's
`build-describe` is that carrier — every dependent reaches its dependencies
through `^describe`, and a dependent's `compile` and `test-exec` sit behind its
own `describe` — so while it declared `**/*.go`, one edited `_test.go` in a
library re-ran 12 cacheable tasks across four projects on a warm cache. It
declares `["**/*.go", "!**/*_test.go"]`, like `build-compile` and
`build-generate`; the same edit now re-runs the owning project's `test` alone.
`test-exec` keeps the test sources, because it runs them. See
[ADR 0043](adr/0043-a-cross-project-carrier-task-keys-only-on-what-it-reads.md)
for why the folded value stays the upstream *key* rather than an output digest
or a closure digest.

A file wrongly excluded from a key serves a stale result; a file wrongly
included costs a rebuild. Narrow a declaration only against the action's own
code, never against an assumption about what it probably reads.

## Cache Lifecycle

### Lookup (on cache hit)

1. Compute the cache key hash.
2. Check if `blobs/{prefix}/{hash}/` exists.
3. Read `result.json` to restore the job result (status, data, error, and compact summary events).
4. If `files/` directory exists, copy cached artifacts to the output directory.
5. Skip subprocess execution.

The skipped task process does not make the hit free: the CLI still computes
input digests, stamps version/capability source bindings, verifies the entry,
and materializes declared outputs. The terminal human summary reports that
local leg as `Cache: … local hit … served in …`, with `keys`, `bindings`,
`restore-verify`, and source-binding child-process counts. Machine consumers
read the same attribution from the aggregated `--output=json` run summary and
recorded `session.json`. The bounded JSONL terminal keeps only verdict-sized run
fields; it points to the complete session artifact instead of carrying this
run-level cache breakdown. `servedMs` is the union of all local cache
intervals, so parallel hits do not multiply elapsed wall. The phase breakdown
partitions that union once with stable priority `bindings`, then `keys`, then
`restoreVerify`; this gives nested source-binding work its own attribution and
leaves the enclosing restore phase only the remaining wall. The phase sum
therefore equals `servedMs`, apart from at most 2 ms lost when the three exact
phase totals are independently truncated to integer milliseconds.

Source bindings share one repository-wide Git enumeration within a scheduler
run. Previously, every distinct capability-package project spawned
`git ls-files --stage -z -- <project>` and
`git ls-files --others --exclude-standard -z -- <project>`. In the 141-task
clientgen provider build, 46 project bindings therefore spawned 92 processes
even while 131 tasks were restored locally. The measured binding cost was
2,326–3,993 ms total, or about 51–87 ms per project binding.

The scheduler now runs those two `ls-files` commands once for the repository
and selects each exact project's entries in memory. The shared snapshot holds
only file enumeration: working bytes, deletions, executable/symlink modes and
visible gitlink commits are still read when deriving a binding. Git's ignore
rules still exclude untracked files only, and a conflicted index entry rejects
only bindings whose project contains it. Source-v1 exclusions, ordering and
hashing remain owned by the capabilities protocol. Gitlink worktree checks
retain their separately counted `rev-parse` processes.

This snapshot is run-local and invalidated with the source-binding memo after
real task execution and around source-visible output restores. A new file,
index update or changed ignore rule is therefore enumerated on the next
binding miss. Restoring project `.gen` outputs keeps the snapshot and bindings:
those bytes are excluded from source-v1. Workspace-rooted restores invalidate
all bindings; project-rooted restores preserve unrelated project bindings.

Fresh runs of `./putnamiw validate --projects @putnami/utils` on the same
unchanged worktree retained the 141 planned / 131 locally restored / 10 executed
task shape. The nested build spent 219–220 ms in bindings with two processes,
against 4,368–4,451 ms of executed-task CPU. The complete clientgen guard still
took 11.8–12.4 s on that machine: the nested build occupied 5.0–5.2 s and work
outside the recorded nested run another 6.8–7.3 s. The inherited five-second
guard target is therefore still unmet; batching removes the per-project Git
process cost without claiming that it eliminates the guard's other work.

### Store (on successful execution)

1. Compute the cache key hash (already done during lookup).
2. Write `result.json` with the job result and replayable summary events (`meta`, `metric`, `artifact`, `diagnostic`, `summary`).
3. Write `meta.json` with metadata (extension, task, project, timestamp).
4. If the output directory has files, copy them to `files/`.
5. All writes go to `tmp/` first, then atomically rename to `blobs/`.

### Failure replay (negative entries)

A task that **fails** is recorded too, in the local store only, under the same
cache key that would have served its success. A later run whose key is
unchanged replays that verdict instead of executing it again:

```
test~test @putnami/cli  ✗  0ms  (replayed — inputs unchanged since 14:02 (12m ago), attempt 3)
    main_test.go:12 assertion failed
    error: 1 failing test
    Nothing changed since the last failure. Fix the cause, or pass --retry-failed to run it again.
```

A replayed failure is a failure in every respect: same `failed` status, same
exit code, the original diagnostics and error logs, the same non-zero session
verdict, and the same effect on dependent tasks. It is **not** a cache hit — it
never appears in the cached count of the run summary.

**Invalidation.** A recorded failure disappears when, and only when:

| Event | Effect |
| --- | --- |
| Any keyed input changes | The key moves; the old record is unreachable |
| `--retry-failed` | The task executes; the new outcome is recorded |
| The task succeeds at the same key | The record is deleted, **under any selection and under a cache bypass** |
| `--no-cache` | The replay is suppressed, and the task executes. That alone removes nothing: only the success it may produce does |
| `--no-cache-projects` names the task's project (or an upstream one) | Same as `--no-cache`, for that task only |
| `cache clean` / `cache gc` | The record is reclaimed like any other entry |

There is **no expiry clock**. A failure that the inputs cannot explain is a
determinism bug, and an expiry would hide exactly the signal worth seeing. See
[ADR 0030](adr/0030-a-failed-task-is-cached-until-its-inputs-change.md).

**A green run clears the record, however you ran it.** A success at a key is
proof that the failure recorded there is stale, so it is deleted whatever the
run's cache policy and whatever it selected. Two consequences are worth naming,
because both used to surprise:

- **A cache bypass suppresses entries, not identity.** Under `--no-cache` or
  `--no-cache-projects` a task still computes its cache key: the run neither
  reads nor publishes anything at it, but a bypassed **success** still deletes
  the failure recorded there. Without that, the one flag a user reaches for to
  get past a stuck verdict was the single path that could not clear it.
  A bypassed **failure** is still refused — its verdict is not describable by a
  key the run declined to read, which is what the bypass means.
- **A task keys the same under every selection.** `--projects <p>` and
  `--impacted` give the same task the same key for the same inputs, so a green
  run under either clears what the other recorded. The selection decides which
  tasks run, never how one is keyed: the planner resolves a task's upstream
  dependencies against the workspace dependency **graph**, not against the
  selection, so the same upstream jobs are planned — and therefore the same
  upstream hashes folded — whichever way the run was narrowed. A project that
  `disable.tags` removes from an `--impacted` selection contributes nothing to
  this: its own tasks are simply not planned there, and a task that is not
  planned produces no replay line to read.

**What is never recorded.** A verdict is recorded only when the cache key
explains it, so these five are always re-executed:

- a **timeout** — a deadline depends on host load, not on the key (the result
  carries a structural `TimedOut` marker; the message is never matched);
- a **sensitive leak** — the guard matches an invocation-scoped artifact's path
  and bytes and the process capabilities a run provisions, none of which the key
  describes;
- a **canceled** task — the session was cut short;
- a **skipped** task — it has its own caching rule above;
- a **wrapper bail-out** that never reached the job's own logic (no `meta`
  event), such as an unavailable toolchain or an empty project.

**Storage and isolation.** The record is a single small JSON file in its own
blob directory, with no declared outputs and no CAS blobs — a failed task's
output tree is untrusted. Its address is derived from a distinct domain, so the
remote-cache code cannot name it: **a negative entry is never published to, or
fetched from, a remote cache.** It carries the ordinary `lastUsed` sidecar, so
it counts against the store byte budget and is evicted by idle reclaim like
every other entry. Like every other entry it lives in the machine-global
per-repo store, so a sibling worktree at the same inputs replays the same
failure — and, like every other entry, it never leaves the machine.

### Task-owned entries (declared outputs)

The lifecycle above is the **inferred** model: what an entry holds is whatever a
walk of the shared per-command output directory found, minus a pre-run baseline,
plus a hand-maintained list of in-tree subtrees (`.gen`, generated clients).
Its shape is a function of run order and of what an earlier session left behind.

A task whose manifest carries a **task contract v3 `declares` block**
([07-extensions.md](07-extensions.md)) takes a second, parallel model: the entry
holds *exactly the declared set*. Per declared output it records the id, whether
it is a file or a subtree, the root it resolves against (`project`, `workspace`,
`command-output`), the root-relative path, and whether the task produced bytes
for it at all — so an output that is legitimately empty is recorded as empty
rather than being indistinguishable from a capture that missed it.

Capture reads each output **from its real location** — the task is not run in a
sandbox — and stages it under a task-owned staging root (hardlinks, or copies
when a link is impossible) before the entry is ingested and the declaration is
validated against what was staged. The staging root is the *store's* boundary,
not the process's: it is what makes an entry independent of sibling steps and of
directory residue. It does **not** police writes outside the declaration, which
would need process-level isolation this workspace cannot take yet.

A directory output may **cede** subpaths (`excludes`): staging skips them, so
the entry holds no bytes under a path the declaration says belongs to another
task. That is what lets the task which writes one subtree of an owned tree keep
it across a cache hit — `@putnami/go`'s `build-generate` owns `<project>/.gen`
and cedes `.gen/migration-bundle` to `build-describe`, which writes it after
generate has already been captured. The ceding task's restore leaves the ceded
subtree exactly as it finds it, the way an execution would: it merges into the
existing directory instead of swapping a tree over it. Each entry the output
owns is replaced in place, each one it no longer has is removed, and the ceded
subtree is never renamed, copied or linked, so its readers and its owner see
nothing happen. The owned entries change one at a time rather than all at once,
which is also what an execution does. Where the filesystem can exchange two
paths in one step (`renameat2` with `RENAME_EXCHANGE` on Linux, `renamex_np`
with `RENAME_SWAP` on macOS), an owned entry that both the old and the new tree
hold is never absent during the restore; elsewhere each owned
directory is briefly absent while it is replaced. An
output that cedes nothing is still replaced as a whole, with the same exchange
when the filesystem has it. In every case, a reader that already holds a
replaced directory sees it emptied as it is reclaimed. This also applies to a
nested pathname lookup that has traversed that directory before replacement:
resolving its next component or reading the retired inode's metadata can fail
with `ENOENT`. Atomic replacement protects the entry, not a read snapshot;
the ceded subtree stays untouched
throughout, including its nested readers. The owning task
still runs after the ceding one, because its restore or execution is what
makes the ceded path match the current sources.

The two models never see each other's payloads, because the entry-format version
is part of the entry's **address**, not a field inside it:

```
address = sha256("putnami/store/entry-format\0" + <format> + "\0" + key)
```

A legacy entry is therefore a clean MISS for the task-owned reader (never a
legacy payload interpreted as a declared one), and a task-owned entry is
invisible to older CLI binaries running from another worktree against the same
machine-global store — which cannot be taught anything. Changing the format
constant moves every address, so a future format needs no migration code: old
entries age out through ordinary GC.

Whether a task takes this path is decided **statically**, from the declaration
and the job shape alone, never from run results — the lookup that happens before
the task runs and the store that happens after it must agree. A task whose
declaration does not cover what the inferred path would have captured stays on
the inferred path in full (and says so at `--debug`).

**No command is excluded.** `package` used to be: every packager also
read-merge-wrote a channel index at the root of the shared output directory, a
path no task could own, so a cache hit restored one packager's artifact and
skipped the merge — and `publish` then skipped a channel that had really been
built. The exception was an opt-in (`cache.restoreMode: all-or-nothing`) that
only the Docker candidate could make, and it left every other packaging task
executing on every run.

The fix removed the write instead of working around it. Each packager records its
channel in `<its own output>/channel.json`, and the index publishers read is
DERIVED over those records, so the record is captured and restored by the same
declaration as the artifact it describes. `<command-output>/metadata.json`
survives with a single owner — the archive packager — as the archive publication
manifest an archive uploader reads; it is a declared output like any other.
`cache.restoreMode` is accepted and ignored (see
[ADR 0038](adr/0038-package-tasks-use-ordinary-declared-capture.md)).

A selected `publish-docker` step reads the typed Docker candidate directly;
a sibling packager's record cannot suppress it, and publish-owned registry
evidence is never part of the package cache entry.

### Declared-output drift (committed generated bytes)

Some declared outputs are bytes the repository **commits**: a generated client,
a schema sidecar. For those, "did the task run" is not the whole question; the
question is whether the committed bytes still equal what the current inputs
generate. Only the writer can answer it, and only at the moment it writes —
anything that runs later in the session reads a tree the generator, or a cache
restore of its entry, has already rewritten.

A declared output may therefore carry `drift: "warn" | "fail"`
([protocols/extension ADR 0004](../../../protocols/extension/doc/adr/0004-a-declared-output-may-police-its-own-drift.md)).
The scheduler takes a digest of the output path **immediately before the task
writes it** and compares it with the bytes present afterwards, on both paths
that write:

| Path | Reference taken | Compared after |
|------|-----------------|----------------|
| executed | in `openTask`, before the preBuild hook and the subprocess | `finalizeExecutedJob` has published the entry |
| restored | in `restoreDeclaredCacheHit`, before the recorded tree is swapped in | the swap and the version restamp |

The two compare the same two things, so a cold run and a warm run reach one
verdict — which is how a fresh CI checkout holding stale committed bytes fails
from the restore path: the entry was published by the run that regenerated
them. A difference is reported as the diagnostic `generated-output-drift`,
naming the added, removed and changed paths (sorted, the list bounded, the
counts complete); under `fail` the task also fails with that code, while its
outputs stay written or restored so the worktree holds the regenerated bytes
to commit.

For a literal `path` the reference is that path. For a `pathFrom` output the
path is unknown before the run, so the reference is a digest of the **project
tree** — dot-directories, `node_modules` and `vendor` skipped on both sides —
and the resolved path is looked up in it afterwards; that walk is what limits
the policy to `pathFrom` outputs under the project root. Ceded subpaths
(`excludes`) are compared on neither side.

Three things never happen. The reference and the verdict are facts about the
worktree the task ran in, not about its inputs, so **neither reaches the cache
key** (`TestDriftReferenceIsNeverACacheKeyInput`). The diagnostic is appended
after the entry is published, so **it never travels in an entry** — the next
hit judges its own worktree. And a drift failure is **never recorded as a
replayable failure** (`recordableFailure`), the same rule a timeout and a
sensitive leak follow: the next run must look again. The `drift` field itself
is part of the task contract, so declaring it moves the task's key once.

### Concurrent cold misses

Worktrees sharing the machine-global store also share computation ownership for
the same missing cache key. After both local and remote lookup miss, the first
eligible requester claims a per-key lease before hooks, output preparation, or
subprocess execution. Sibling requesters wait for its atomic publish and then
restore that entry instead of running the job again.

The lease is an optimization, never a correctness dependency. A heartbeat keeps
long-running owners live; an expired or released lease lets exactly one waiter
take over, while a bounded wait, cancellation, or coordination error falls back
to normal local execution. Jobs with a known historical duration below the
cache policy's 200 ms break-even floor bypass leases because coordinating them
would cost more than the duplicated work; jobs without history remain eligible
so fresh worktrees can coalesce their first cold run.

The wait ceiling follows the job timeout multiplied by its configured retry
attempts (five minutes per attempt by default). Lease expiry normally wakes a
waiter earlier; the ceiling exists only so a live-but-wedged owner cannot make
coordination less reliable than independent execution.

### Concurrent sessions in one workspace

Two sessions in the same worktree can schedule the same project task with
different flags. Their cache keys differ, so the lease does not coalesce
them, yet both write the same output directory. A task-output lock keeps
them apart: a task execution never reads or writes its outputs while another
session's task writes them.

Each task locks:

- its command-output directory, `.putnami/out/<project>/<command>`, always;
- its project, when it declares a project-rooted output (such as `.gen`);
- the workspace, when it declares a workspace-rooted output.

A task holds these locks across a cache restore into its outputs, and for an
execution from the output preparation through the capture into the cache:
the version-stamp refresh, the preBuild hook, the subprocess with its
retries, and the capture.

An executing task also locks its project for its preparation alone when the
preparation writes the project tree: the session has not yet started its
extension's preBuild hook for that project, or has not yet refreshed the
project's `.gen/version.json`. The task releases that lock before its
subprocess starts, unless its declared outputs need it, so a long-running task
such as `serve` does not keep another session's preparation out of its
project.

A task takes its locks in one order shared by every session: command-output
directories first, then projects, then the workspace. A task that waits for
another session's command output therefore holds no project lock meanwhile,
and two top-level sessions never wait for each other. Tasks
of one session share their locks, so the lock never changes the order within
a session. When another session holds a lock, the task prints one line
to stderr that names that session and its tasks that hold the lock, then
waits:

```
putnami: /tooling/cli:test~test waits for session 20260925-192012-876307 (pid 4242, task /tooling/cli:test~test), which is writing .putnami/out/tooling/cli/test
```

When several tasks of that session share the lock, the line names the first
one and counts the rest (`task /tooling/cli:test~test and 2 more`). The line
never names a process that is gone; it then says `another putnami process`.

The wait has no time limit and ends on Ctrl-C. A task that another session
delays waits instead of failing, with one exception below. A nested session
that a task or its preBuild hook starts in the same workspace inherits the
locks the task holds at that moment, so it never waits for its own parent.
It still waits for any other session: when that other session in turn waits
for a lock the parent task holds, neither progresses until one is canceled
or the parent task's own time limit expires. The parent task then fails with
a timeout, and the wait line printed before names the other session.
The lock files live in `.putnami/locks/task-outputs/`. No input pattern or
declared output covers that directory. The lock is not a cache input. On
hosts without `flock(2)` there is no lock.

The version stamp a session seeds before it computes any cache key takes no
lock. The seed changes `.gen/version.json` only when the revision, the dirty
state or a source binding changed since the stamp was written. Once one
session wrote the current stamp, another session over the same worktree state
writes nothing. When a seed does write, another session that reads or
captures that `.gen` at the same moment can see the new stamp or a partial
one. A cache restore re-stamps what it restores.

### Source-writing fix tasks

A task can be keyed by an *unfixed* tree and then rewrite it — `golangci-lint
--fix`, `biome` in fix mode, TypeScript `lint --fix`.

A v3 task that declares `mutatesSources` (inseparably paired with the `sources`
write resource) uses a **clean-only status entry**. After a successful
execution, Putnami invalidates its input-digest memo and recomputes the digest
of that task's keyed source files:

- If the source digest is unchanged, the fixer made no keyed source edits. Its green
  status is reusable, so an unchanged warm `lint` can be served entirely from
  cache.
- If the source digest changed — or equality cannot be proved — the entry carries a
  cache-private mutation marker. Local, remote, and coalesced restore paths all
  reject that entry and execute the fixer in their own worktree.

The detector reads the file patterns from the same place the cache key does, so
it can never observe a narrower set than the key trusts: the task's declared key
files (or the job's `filePatterns`), the command's `filePatterns` flag, the
project's option layers, and the workspace-relative patterns. It also reads
every project config the key covers as text, as the key does, so a layout-only
rewrite of `putnami.json` counts as a source edit. A rewriter that
declares no project-relative patterns at all cannot be proven clean — its key is
blind to what it rewrites, so every result it publishes is marked, rather than
compared against a constant that never moves.

This preserves the useful part of caching without handing back a formatter's
verdict unless the same source bytes previously needed no edits. A contract-v2
task has no declaration from which to select task-owned capture and remains
uncached after the v3 cutover.

The source-rewriter task identity versions this detector separately from
ordinary task keys. An older, more conservative mutation marker therefore
cannot permanently shadow a clean result written by a newer detector.

Either way no source bytes are ever replayed out of a shared or remote entry.

Waiters that restore the owner's publish report **`coalesced`**, not `cached` or
`success`. This is a run-local outcome and is never persisted inside the cache
entry itself. It appears in live/text output, `session.json` as
`run.reuse.coalesced`, `sessions inspect`, structured output summaries, and
trace profiles as a `cache-coalesced` instant event with job-end status
`coalesced`. Ordinary warm local or remote hits remain `cached`; the process
that owned and executed the cold miss remains `success`.

A second arrangement reports the same outcome, and it is the same statement about
provenance: a **plan-level shared node**. When several commands schedule
content-identical work over one manifest task, the planner groups those nodes and
the first one to miss executes; the rest adopt its result and report `coalesced`
(see *Cross-Command Shared Executions* in
[04-job-execution.md](04-job-execution.md)). Each member still computes its own
key and publishes its own entry, so this changes what a **cold** run spends and
nothing about which addresses hold what.

### Atomic Writes & Cross-Process Safety

The store is shared by many concurrent processes (one per worktree), so writes are both atomic and **first-writer-wins**:

1. Create the entry under `tmp/{uuid}/` and ingest its files into `cas/`.
2. `os.Rename()` the directory to `blobs/{prefix}/{hash}/` — **without** removing any existing entry first.
3. If the destination already exists (a sibling published this byte-identical entry first), keep theirs and discard the staged copy.

Because entries are content-addressed, the loser of a publish race holds an identical copy, so never clobbering is safe — and it means a reader following a `.putnami/out` symlink into an entry is never yanked out from under it by a concurrent publisher.

Publishers hold the store's `.lock` in **shared** mode for the whole ingest+publish; garbage collection takes it **exclusive**, in short sections (see [Garbage Collection](#garbage-collection)). So under the GC lock no entry is mid-ingest, which is what makes the CAS orphan sweep (below) sound. The in-process mutex still serializes goroutines within a single process. On platforms without `flock(2)` the lock degrades to single-process safety (the primary worktree target is darwin/linux).

If the process crashes between steps 1 and 2, the partial entry in `tmp/` is cleaned up on the next run.

## Cache Stores

### Local Store

The default store. Writes to the machine-global per-repo store (see [Store Location & Sharing](#store-location--sharing)).

- Concurrent-safe across processes via an advisory file lock (shared for publishers, exclusive for GC), plus an in-process mutex.
- Bounded by a global byte budget enforced by [Garbage Collection](#garbage-collection).
- Recently-used entries (within the GC grace window) are never evicted, protecting in-flight readers.

### Chained Store

Combines local and remote stores:

1. **Read**: Check local first. On miss, check remote. Promote remote hits to local.
2. **Write**: Write to both stores.
3. **Prune**: Only prune local store.

### Remote Store

The CLI can share the cache across machines and CI runs through a remote build
cache server. It is opt-in and configured per workspace. See
[Remote Build Cache](#remote-build-cache) below.

## Remote Build Cache

The remote build cache shares cached results across machines and CI runs. It is
**off by default** and **opt-in**: a build uses it only when a configuration is
present and enabled. When active, the whole build's cache keys are negotiated
with the server in a single round trip before execution; remote hits are
restored into the local store (so a later local lookup hits without the network),
and freshly built misses are uploaded.

Cached results preserve the compact events that feed the live inline summaries:
test counts, coverage, generated file/artifact counts, warning/error counts, and
visible summary labels. Streaming logs, phases, and progress are not cached.
Remote servers advertise `action-events` before the CLI sends those events in
commit payloads; servers that support it persist and return them with hits.

The remote cache is strictly **best-effort**: any provider, negotiate, download,
or upload failure is logged and the build falls back to local behaviour. It can
only accelerate a build, never break it.

### Successful run markers for CI defaults

Remote caching also carries a separate successful-run marker used by smart bare
project selection on `main`/`master`. A fresh CI checkout often lacks the local
`.putnami/sessions/last-build.json` marker; when remote caching is active,
`putnami build` first checks the local marker, then asks the remote cache for a
same-branch, same-command-list, same-parameter marker. If the returned SHA
resolves in the local checkout, Putnami selects impacted projects vs that SHA.
If the marker is missing, stale, invalid, or the remote cache is unavailable,
the command falls back to all projects.

This marker is not derived from action-cache hits. A set of per-job cache hits
does not prove that the whole requested target passed together at one commit.
The CLI publishes the marker only after a fully successful all-project run or a
successful smart-selected full-target run, and publish failures are best-effort.

### Enabling and configuration

Remote caching requires a capable `@putnami/cloud` cache-provider extension and
an explicit opt-in. Core only reads enough configuration to decide whether to
delegate to the provider and which materialization mode to request; credentials
and server-specific auth remain provider-owned.

The opt-in is configured by a per-workspace file — `.putnami/cache.json` — that
the cloud extension writes, or by environment variables (for CI):

```json
{
  "enabled": true,
  "url": "https://cache.putnami.cloud",
  "mode": "full"
}
```

| Field | Meaning |
|-------|---------|
| `enabled` | Gates remote caching. Omitted ⇒ enabled; set `false` to disable without deleting the file. |
| `url` | Cache server base URL. Remote caching is inactive when empty. |
| `mode` | Materialization mode: `minimal`, `toplevel`, or `full` (default `full`). |

Legacy files may still contain a `token` field. Core ignores it; the in-core HTTP
client that consumed token recipes has been removed. The cloud provider may read
its own configuration, but core does not execute token commands or fetch token
URLs.

Environment overrides (useful in CI, no file required):

| Variable | Effect |
|----------|--------|
| `PUTNAMI_CACHE_URL` | Overrides `url`. |
| `PUTNAMI_CACHE_MODE` | Overrides `mode`. |
| `PUTNAMI_CACHE_TOKEN` | Passed through to the provider process for CI credential use; core never reads it. With [`--credential-fd`](03-commands.md#run-credential---credential-fd), the CLI removes it from its environment first, so no process it starts receives it, and hands the run credential to the provider through `authenticate` instead (see [Authentication](#authentication-per-user-token)). |
| `PUTNAMI_CACHE_TRUST` | Sets remote-entry trust to `ci`, `any`, or `none`. The resolved value is also exported to every job (see [Object cache](#object-cache-for-compiler-caches)). |

Local caching must be on (no `--no-cache`) for the remote cache to apply.

### Trust policy and provenance

Every provenance-aware provider hit is classified as `trusted` or `hint` from
the provider's authenticated identity. The client-supplied producer fields are
not authoritative: the provider derives and overwrites them before persistence.
Use `--cache-trust` to choose which remote entries can satisfy this run:

| Policy | Remote behavior | Default |
|--------|-----------------|---------|
| `ci` | Only `channel=trusted` can satisfy a job. Hints and channel-less legacy entries may warm verified CAS blobs, but the job always re-executes. | CI |
| `any` | Trusted, hint, and legacy entries may satisfy jobs. | Local development |
| `none` | Ignore the remote provider completely; the local store still applies. | — |

Resolution is explicit and source-aware: `--cache-trust` wins over
`PUTNAMI_CACHE_TRUST`, which wins over the command's workspace option
(`options.<command>.cache-trust`), then the environment default. For a
multi-command run, differing command options combine to the strictest policy.
`publish`, `tag`, and `version tag` force at least `ci` even when `any` was
requested; `none` remains a stricter opt-out.

Older providers do not return a channel. Under `any` they retain their previous
behavior. Under `ci` their entries are treated as hints, so they cannot turn an
authoritative run green.

Remote successful-run markers do not carry provenance yet. Putnami therefore
uses them for bare `main`/`master` project selection only under `any`; under
`ci`, selection falls back to a local successful-run marker or all projects.

### Authentication (per-user token)

Every request carries a **per-user bearer token**; there are no shared secrets,
and the token is **never written to disk** by core. The `@putnami/cloud`
cache-provider resolves credentials lazily inside the provider process, using
its own sign-in state or inherited CI environment such as `PUTNAMI_CACHE_TOKEN`.
Core launches the provider with its inherited environment and never handles the
bearer directly.

A hosted run (`--credential-fd`) is the exception. The engine removes
`PUTNAMI_CACHE_TOKEN` from its environment, so neither the provider nor any
task inherits it, and hands the run credential to the provider over the
provider RPC: it lists the `run-credential` capability at `initialize`, and a
provider that echoes it receives the credential in one `authenticate` request
before any other op. A provider that does not echo it receives nothing. A
refused `authenticate` makes the run build locally, like a failed
`initialize`. Locally, without `--credential-fd`, nothing changes. The
[cache provider RPC](16-cache-provider-rpc.md) documents the op.

If a cache is configured (`.putnami/cache.json` or a `PUTNAMI_CACHE_*` override
is present) but no capable provider is available, the build falls back to
local-only and prints a one-line notice pointing at `@putnami/cloud`. It is never
silent: a configured-but-unusable cache always reports why it degraded. The plain
unconfigured case (no file, no env) stays quiet, even when `@putnami/cloud` is
installed, because local-only is the expected default there rather than a
degradation.

Provisioning this configuration — authenticating the user and writing
`.putnami/cache.json` — is owned by the cloud extension's setup/login commands.

### Materialization modes ("Build without the Bytes")

A remote hit's output bytes are fetched lazily — only when something actually
needs them. The mode tunes how much is materialized:

Concurrent worktrees coalesce an eligible remote restore before invoking their
session-private cache providers: the first process downloads the entry's missing
CAS blobs and publishes it into the machine-global store, then waiters restore
that shared entry without a second provider download. The same 200 ms historical
job-cost floor as computation leases skips known-cheap work, while an expired
lease or bounded wait falls back to a direct provider restore so coordination
cannot make the cache less reliable.

| Mode | Behaviour |
|------|-----------|
| `minimal` | Status only (the CI gate). Bytes move only when a local build consumes them as an input. |
| `toplevel` | Materialize requested deliverables. |
| `full` | Materialize every hit's outputs (local development; the default). |

### What is and isn't cached remotely

Remote caching is stricter than local:

- **Side-effecting tasks are never remote-cached** (e.g. `publish`) — their effect must always run.
- A **break-even guard** skips uploading artifacts whose predicted transfer time would exceed the build time they save (using the recorded build duration and output size).
- All transfers are content-addressed and **digest-verified**; bytes that do not match their digest are rejected.

### Object cache (for compiler caches)

The task cache skips a whole task. It cannot help *inside* one: a compiler that
starts with an empty cache of its own recompiles everything a task does run, on
every fresh machine. The **object cache** is the generic surface that closes
that gap — small opaque payloads addressed by `(namespace, id)`, served by the
same cache provider over a local Unix socket, so a job process reaches it
directly instead of going through core.

Core stays language-neutral. It negotiates the socket and exports two variables
to every job it spawns:

| Variable | Meaning |
|----------|---------|
| `PUTNAMI_CACHE_OBJECT_SOCKET` | Absolute path of the provider's object-cache socket. Its **absence is the off switch**. |
| `PUTNAMI_CACHE_TRUST` | The run's resolved trust policy (`any` or `ci`), so a job filters objects by the same rule core applies to task entries. |

A job that sees the socket speaks the provider RPC on it: `object-get` to look
up a batch of ids, `object-put` to offer a batch whose bytes it staged in the
exchange directory (the socket's own parent directory). Both are best-effort,
puts are fire-and-forget, and a miss is indistinguishable from an object the
trust filter rejected. Full contract:
[`16-cache-provider-rpc.md`](16-cache-provider-rpc.md).

The two variables are **execution-only**. They never enter a cache key, a run
marker, or task parameters — the socket lives in a per-run temporary directory,
so a key that folded it in would miss on every run.

They are absent when:

- the run has no provider (the ordinary local-only build),
- `--no-cache` is set: a run that consults no cache hands its jobs none either,
- `--cache-trust none` disables the remote cache entirely.

Because a job can need the object cache when there is nothing else to warm, the
provider now starts whenever at least one planned job will execute locally, not
only when the run has remote keys to negotiate. A fully warm rebuild — every
planned job a local hit — still starts no provider at all.

### Scoping the bypass to some projects

`--projects <p>` plans `p` **and its whole dependency closure** — a project
cannot build against unbuilt dependencies. `--no-cache` beside it therefore
recompiles that closure cold, even when the reason for the flag concerns `p`
alone.

`--no-cache-projects <selector>` is the scoped form:

```bash
putnami build --projects /services/catalog --no-cache-projects /services/catalog
```

| | `--no-cache` | `--no-cache-projects <selector>` |
| --- | --- | --- |
| Named projects' tasks | Re-executed | Re-executed |
| Their dependency closure | Re-executed | Served from cache |
| Their planned dependents | Re-executed | Re-executed (see below) |
| Everything else in the run | Re-executed | Served from cache |
| Remote cache | Not consulted at all | Consulted for the projects that keep it |
| Extensions receive `cache: false` | Yes (when typed by the user) | No — the bypass is per project, the parameter bag is per run |

Rules:

- The selector speaks the same grammar as `--projects` (project id, package
  name, path pattern, alias, group).
- Tag filters, `--exclude` and the workspace's default tag exclusions do **not**
  apply to it: it is a cache-policy target list, not the run's scope, so a
  project the workspace excludes by default stays nameable.
- A selector that matches **no** project fails the run with a usage error. The
  flag exists to take a project out of the cache; a typo that silently left it
  in would serve a cached verdict for exactly the task you asked to re-derive.
- The bypass extends to every planned task that transitively depends on a named
  project, over cache-key edges. A bypassed task re-derives its output from
  source, and that output may differ from what the entry at its key holds — so a
  dependent left cacheable would key identically and be served a verdict
  computed against the **previous** upstream content, which is exactly the
  reuse the flag exists to refuse. Write-serialization edges do not extend it —
  they never contribute to a key.
- A bypassed task is never looked up, never served (locally or remotely), never
  published, and never replays a recorded failure — and a bypassed **failure**
  records nothing. The one store write it may make is a DELETE: a bypassed
  success removes a recorded failure at its key, which is stale by the fact that
  the task just passed at those inputs. It keeps the exact key it would
  otherwise have had: the flag is execution policy, like `--max-parallel` and
  `--retry-failed`, and nothing derived from it reaches a cache key, a run
  marker, or task parameters.
- `--no-cache` subsumes it. When both are given, the run-wide flag wins.

`@putnami/clientgen`'s `clientgen-sync` is the first caller: it materializes
its providers' contracts with
`build --projects <providers> --no-cache-projects <providers>`, so the providers
are re-derived from source while the 90 %+ of planned tasks that are merely
their dependencies stay cached.

The bypass there is whole-project, and stays that way. Narrowing it to only the
tasks that produce the contract sync reads was measured and rejected:
the tasks a narrowing would restore are 2.6 s of processor time and 1.0 s of
that nested run's 2.3 s critical path, while the run's own floor — planning 141
tasks and restoring the 131 cached ones — is larger than both. A task-scoped
bypass would also need a second closure rule, over task edges rather than
project edges, for the same flag. That floor is also why the `validate` guard
no longer spawns this build at all: whether a committed client is what
the current contract generates is judged by the scheduler on the generator
task's declared output (see "Declared-output drift"), and the guard reads
committed inputs only.

### Speculative prefetch

After negotiation, the CLI prefetches in the background — in parallel — the
hit-dependencies of every miss (the cached inputs a local build is about to
consume), so a job that must build finds its inputs already materialized instead
of blocking on them. A blob is fetched at most once, whether prefetch or the
job's own restore reaches it first.

## Cache Control

### Disabling Cache

```bash
# Skip cache for this run (entries are kept for future runs)
putnami build --no-cache

# Skip cache for these projects only; everything else in the run keeps it
putnami build --projects /services/catalog --no-cache-projects /services/catalog

# Re-run tasks whose identical failure is cached, keeping every other cache hit
putnami test --retry-failed

# Ignore only the remote provider; keep using the local store
putnami build --cache-trust=none

# Delete this repo's cached data (shared across all its worktrees)
putnami cache clean

# Delete every repo's cached data on the machine
putnami cache clean --all

# Garbage-collect the global store down to its byte budget
putnami cache gc

# Verify deterministic keys, captured artifacts, write ownership, and restores
putnami cache verify --impacted
```

Because the store is shared across all worktrees of a repo, `cache clean` clears the cache for **every worktree of the current repo**, not just the current one. It runs under the store's exclusive lock, so it waits for in-flight sibling builds to drain.

Cache subcommands do not support `--dry-run`. Passing it returns a usage error
before any store is mutated, extension cache hook is invoked, or verification
worktree is created.

### Cache Determinism Verification

`putnami cache verify` audits the canonical `lint,test,build` plan using the
ordinary engine, scheduler, cache-key builder, capture, and restore paths. It
does not define a second cache protocol and does not alter cache keys. The
command requires a clean worktree, creates three detached temporary worktrees,
and runs tasks serially so each task's filesystem observation has one owner:

1. Two live runs use independent empty stores. Equivalent tasks must compute
   the same key and publish the same declared-output manifest and bytes.
2. A third pristine worktree reuses the first store. Cacheable tasks must hit,
   and its final project tree must match the first live tree.
3. Each live task's workspace-tree changes must stay within the **project's**
   declared project or workspace outputs — the union over every task planned
   for that project, not the writing task's own outputs alone. That matches the
   one-owner-per-output model the manifests declare: `build-generate` is the
   single producer of `<project>/.gen` and declares that subtree whole minus
   the subpaths it cedes (to `build-describe`, `.gen/conf` to the two
   `config-merge` tasks, which declare the merged file inside it, and
   `.gen/deployment.json` to `package-deployment`, which declares it), while
   `build-describe`'s staging and `build-infra`'s requirements write inside it
   and declare nothing. What the
   check still catches is a write that escapes the project's declared surface
   entirely, or one project's task writing into another project's tree. A task
   that declares `mutatesSources` may also change its own keyed source inputs —
   that licence belongs to the writer and is never inherited from a sibling.

Keys that explicitly depend on ambient environment or runtime inputs are
reported as `ambient` **when the two live runs differ**; that key difference
alone does not fail verification. Such a task's keys are still compared like
every other task's, so the summary's "N with declared ambient inputs" counts
declarations, not comparisons that were skipped. Captured-byte drift, an unreadable/missing entry,
an undeclared write, a missing hit, or a non-ephemeral live-versus-hit tree
difference is blocking. Use `--output=jsonl` for the versioned report containing
per-task check states, classified `.gen` paths, findings, and summary counts.

Every observed `.gen` path must have exactly one effective role. Putnami derives
`keyed-input` and `captured-output` roles from the existing task contracts and
treats its existing generated version, agent-context, and per-project infra
manifests as `ephemeral`. A project can classify a provider-owned exception in
`putnami.json` without changing cache keys or entry formats:

```json
{
  "options": {
    "cache-verify": {
      "keyed-inputs": [".gen/schema/**"],
      "captured-outputs": [".gen/client/**"],
      "ephemeral": [".gen/local-trace.json"]
    }
  }
}
```

Overrides are project-relative and must remain under `.gen`. Keep them narrow:
`ephemeral` paths are excluded from hit/live equality and accepted by the write
closure, while the other two roles remain checked against the task that owns
them. `--projects`, `--impacted`, `--filter-tag`, and `--all` select the audited
projects; with no selector, the command verifies all projects.

### Per-Task Cache Policy

Extensions control caching via the task manifest:

```json
{
  "cache": {
    "enabled": true,
    "deterministic": true,
    "key": {
      "files": ["src/**/*.ts", "tsconfig.json"],
      "env": ["NODE_ENV"]
    }
  }
}
```

Setting `"enabled": false` disables caching for that task entirely. Use this for
side-effecting tasks (e.g. `publish-*`) so a local cache hit can never skip an
external effect such as a registry upload.

### Tasks a cache key cannot describe

`"enabled": false` also covers a second family: a task whose verdict depends on
something the key provably cannot name. A key that is *almost* complete is worse
than no key, because the task is then served a stored verdict that was reached
against different inputs, and it reports success for a workspace it never
examined. Three shapes recur.

**A per-run reference.** A value that is different on every run — a temporary
path, a session id — in a task's environment or params is a per-run identity:
a permanent miss for that task and every node sharing its execution. Run-level
values therefore travel on the scheduler's execution-only channel
(`jobProcessEnv`, `PUTNAMI_PARENT_SESSION_ID`) and never through the hashed job
context. The engine's own per-run references never reach a task at all: the
drift reference of "Declared-output drift" is taken and compared on the
engine's side of the task boundary (the session artifact baseline that used to
be handed to an uncacheable workspace verifier as a per-run path is gone, ADR
[0034](adr/0034-a-declared-output-polices-its-own-drift.md)).

**An input the task itself produces.** A cache key is computed immediately
before the job runs. A task that builds or generates one of its own inputs
mid-run therefore hashes the PREVIOUS run's bytes if it names that input in
`cache.key.files` or `cache.key.workspaceFiles`. The result is a key that does
not move when the thing it is supposed to describe changes. Name the settled
sources that derive the input, or nothing.

**An unbounded read set.** A workspace-wide verifier reads every project's
sources. `cache.key.workspaceFiles` can express that — the patterns resolve
against the workspace root, and `lookupFileHash` memoizes the digest per CLI
invocation, so the hashing itself is affordable. What is not affordable is the
review: a pattern list over a whole workspace cannot be shown complete, and the
first file it misses is a silently stale verdict.

`@putnami/clientgen`'s `clientgen-workspace-check` — the task `putnami validate`
runs as `clientgen-guard` — has the third, and is `cache: false` for that
reason. It used to have all three: a per-run pre-session capture and a
contract its own child build produced. Both are gone: drift is the
generator task's verdict under "Declared-output drift", and the guard reads
committed inputs only. Its cost is bounded by doing less work, not by storing
the answer.

### Version-Aware Tasks

Most tasks are **content-oriented**: their cache key tracks source files,
params, and upstream hashes, but not the release version — so a build does not
re-run just because the workspace release id moved.

Tasks that **stamp the publish version into their output** must opt out of that
by declaring `"versionAware": true`:

```json
{
  "cache": {
    "versionAware": true
  }
}
```

This mixes the full publish version (base semver + per-commit suffix) into the
cache key. Two runs with identical source/build inputs but a different version
suffix then **miss** and re-package with the correct version, while re-running
on the same commit still **hits**. It applies to library package artifacts —
npm packages (`package-npm`), Go modules (`package-go`), and version-stamped
archives (`package-archives`, `package-content`). Go binaries that inject the
version via `-X` ldflags get the same effect from a non-empty `version-var`
param instead. Docker packagers remain content-addressed: their image
stamp intentionally omits the publish version, SHA, and branch; manifest version
metadata is advisory, and publish resolves the session version independently.
Extension authors should not mark Docker packaging `versionAware` merely to
carry the publish version.

### The Build Stamp Is Not a Cache-Key Input

Release planning, execution and cached verification-report recovery share one
per-run version map. It combines the pre-hook tree identity with each release
line's version, resolved during planning. Later stages neither repeat the Git
history traversal nor observe a mid-run tag change that would move their cache
keys. A new run, including a watch iteration, resolves a fresh map; watch retains
its existing pre-hook tree snapshot so generated writes do not become a false
dirty suffix.

`<project>/.gen/version.json` records the revision a project was built at. The
scheduler seeds it for every planned project *before* any cache key is computed,
generation tasks own the `.gen` subtree that contains it, and packaging, publish
and deploy read it as the release identity. Two rules keep those facts from
turning every commit into a cold build:

- **Generation stays content-oriented.** Writing the project `gen` resource does
  *not* make a task version-aware. Generation output derives from declared
  content, so a new SHA on an unchanged tree must reuse it — otherwise the new
  key propagates through every downstream key that mixes it in and the whole
  compile / test / describe closure goes cold on a metadata-only commit.
- **The stamp is re-materialized after a cache hit.** A restored `.gen` subtree
  carries the *producing* run's stamp, so the scheduler rewrites
  `.gen/version.json` at the current identity immediately after the restore and
  before the hit is published — on the local, remote and coalesced legs alike. A
  deploy that reads the file therefore always sees the revision it is deploying.
  The rewrite is skipped when every identity field already matches, so an all-hit
  run keeps the build time of the run that produced its artifacts.

When a task's own input globs reach the stamp (TypeScript lint's `**/*.json`
does), it is hashed with `buildTime` blanked: the build time describes the
invocation, not the tree, and hashing it would let a run invalidate the very key
it was computed under.

A workspace root Git does not manage has no source binding: no `git` program is
on `PATH`, or the root is outside every repository. The scheduler stamps each
capability package there with an empty `sourceBinding` and
`sourceBindingUnavailable: true`, and the capability producers emit their
manifest without feature evidence. The stamp is not a declared input of those
tasks, so the key itself carries the state: every task key of such a root
includes a `sourceState` marker, and an entry built in one state never serves
the other. A key computed inside a repository carries no marker and keeps its
address. A publication's selection fingerprint never carries the marker.

#### The stamp is a shared document

The scheduler owns exactly the identity fields — `name`, `version`, `suffix`,
`sha`, `branch`, `isDirty`, `buildTime`, `capabilityRoot`, `capabilityPackages`.
**Every other top-level key belongs to whoever added it and is carried through
untouched** by both the plan-time seed and the post-hit re-stamp. Extensions
depend on this: the TypeScript generator merges in `contentHash` (which the
running app reads back through `getBuildInfo()` to namespace its disk cache), and
docker publish overlays a `publish` object. Extension authors writing to the
stamp must merge into the document rather than replace it, exactly as the CLI
does — a whole-file rewrite destroys the other writers' fields, and after a
generation cache hit nothing re-runs to restore them.

An identity field the current build leaves empty is *removed* rather than
inherited, so a `suffix` from the previous commit can never linger and be read as
this build's identity.

### Watch Mode

Watch mode automatically disables the cache (`--no-cache`) to ensure fresh results on every file change.

## Garbage Collection

GC keeps disk use in check without manual pruning, on two axes — a **size budget** and a **use-based idle reclaim**:

1. **Enumerate** every entry across all per-repo stores under `~/.putnami/store`, each under its store's shared lock, summing real disk usage (the deduplicated `cas/` bytes plus per-entry metadata) and reading each entry's `lastused` (time + generation) and manifest blob digests.
2. **Idle reclaim** — drop entries not hit in the last `PUTNAMI_STORE_MAX_IDLE_BUILDS` builds (see [Build generations](#build-generations)), **regardless of budget**, so abandoned entries don't linger in an under-budget store. Entries used within the **grace period** are always spared (a live reader may still hold them).
3. **Budget eviction** — if total usage still exceeds the budget, select more victims oldest-first by `lastused`, down to a low watermark (80% of the budget). Reclaim is accounted in *deduplicated* bytes: a blob shared by several entries is credited as freed only when its **last** referencing entry is selected (digests are refcounted across entries), so shared blobs don't cause under-eviction. Grace-protected entries are skipped here too.
4. **Evict + sweep** under each store's exclusive lock: remove the selected entries, then delete every `cas/` blob not referenced by a surviving entry's manifest. The exclusive lock guarantees no blob is mid-ingest, so the surviving-manifest set fully describes the live blobs — the sweep is portable (it does not rely on hardlink counts) and also reclaims pre-existing orphans from crashes.

   Every lookup, publish and restore on a store waits while its exclusive lock is held, in every session sharing the store. GC therefore holds it in **sections of about 200 ms**, not for the whole pass. A selected entry leaves the store by one atomic rename into `tmp/`, and its tree is deleted after the lock is released. The live set comes from the manifests the enumeration already read; each section that deletes first re-reads the entries published since then, so a blob a publisher linked between two sections is kept. Measured by `BenchmarkEvictStore_ExclusiveLockHold` on a loaded laptop while evicting a tenth of the entries: a single hold used to last 0.9 s (1,000 entries, 10,000 blobs), 2.8 s (3,000 / 30,000) and 5.9 s (10,000 / 50,000). With sections, the longest single hold is 0.2 s, 0.2 s and 0.4 s, for a total of 0.2 s, 0.8 s and 3.2 s spread across the pass.

### Build generations

Each store carries a monotonic **generation** counter, bumped once per cache-using `putnami` run that touches it. Every cache hit (and freshly-built entry) records the generation it occurred in, so "idle" is measured in **builds**, not wall-clock — an idle machine (e.g. over a holiday) does *not* reap a cache that's still valued by your activity, and a busy machine reaps abandoned entries quickly. Idle reclaim only considers entries with a recorded generation; pre-existing entries from before this feature are reclaimed by the size budget alone until their next hit.

GC runs three ways:

- **`putnami cache gc`** — explicit, on demand (blocks on each store's lock as needed).
- **During a run, at batch boundaries** — each time a dispatch group finishes, the scheduler compares the projected usage with the budget: the usage recorded in `.gc-usage` plus the bytes this run has added to the CAS. The comparison reads a counter, not the disk. When the projection exceeds the budget, a pass starts in the background with idle reclaim off. It spares every entry the run has used since it started, on top of the grace window, so no job of the run loses an entry it has looked up, restored or published. The scheduler never waits for a pass and a pass never fails the run, but the run's jobs do wait for it: while it evicts, it holds the store's exclusive lock in sections of about 200 ms (step 4). It retries a busy store's lock for up to 2 s, then skips it. A pass that skipped a busy store records nothing and is retried at the next boundary, up to three times in a row, then after every further tenth of the budget of growth. A pass that finished still over budget (what is left is protected) is retried after another tenth of the budget of growth. The run waits for a pass before exiting, and the session record's `cache.storeBudget` member reports the passes, the entries evicted, the bytes freed, the stores skipped busy, and the total and longest lock hold. Skipped under `--no-cache`, and for a cache verification's temporary store.
- **Opportunistically after a build** — throttled to at most once per machine per hour, and **non-blocking**: it skips any store whose lock is currently held by a sibling build. Sibling builds wait on a store while it holds that store's exclusive lock (in sections, as above). A complete pass records the usage it measured in `.gc-usage`, beside the throttle stamp; a pass that skipped a busy store records nothing, because its measurement is missing that store. Skipped under `--no-cache`.

A run that ends with bytes no pass measured adds them to `.gc-usage`, under a lock shared with every other writer of the file, so consecutive runs within the hour see each other's growth; the next complete pass overwrites the sum with a measurement. The record is created only by a pass: a run's growth alone never becomes the machine's usage.

A pass during a run reclaims only entries the run has not touched. On a fresh disk, where every entry is the run's own, it has nothing to evict until the next run.

> **Grace must exceed your longest build.** The grace window is the only thing protecting a long-running reader in another build (for example, one whose `.putnami/out` symlink points into an entry) from eviction, because readers don't hold a lock for their whole lifetime. If a single build can run longer than `PUTNAMI_STORE_GC_GRACE` (default 1h) while the store is over budget, raise the grace (or the budget) so in-flight artifacts are never collected mid-build. A build's own in-run passes do not depend on the grace: they spare everything that build has used since it started.

| Variable | Effect | Default |
|----------|--------|---------|
| `PUTNAMI_STORE_MAX_BYTES` | Global byte budget across all per-repo stores. | 10 GiB |
| `PUTNAMI_STORE_GC_GRACE` | How recently an entry must have been used to be spared (and the in-flight-reader protection window). Accepts a Go duration (e.g. `30m`, `2h`). | 1h |
| `PUTNAMI_STORE_MAX_IDLE_BUILDS` | Evict entries not hit in this many builds, regardless of budget. `0` disables idle reclaim. | 100 |

These three GC settings are also configurable in the `store` section of `~/.putnami/config.json` / `putnami.workspace.json` (precedence: env > workspace > global > default) — see [Configuration → Build store settings](05-configuration.md#build-store-settings). The store **location** is env-only (`PUTNAMI_STORE_DIR`).

A `lastused` stamp is written on every cache hit (local or remote), so frequently-used entries naturally outlive idle ones.

## Extension-owned machine caches

Everything above is **core's** cache. The language caches below are **not**: each
is created, located and collected by the extension that owns it.

Core's whole knowledge of them is two declarations:

- **`extension.cacheRoot`** — every job context carries one stable machine-global
  directory the running extension owns, exported to subprocesses as
  `PUTNAMI_EXTENSION_CACHE_ROOT`. It defaults to
  `~/.putnami/cache/extensions/<extension>` (override the parent with
  `PUTNAMI_EXTENSION_CACHE_DIR`). Core creates it and never looks inside.
- **The reserved `cache-clean` / `cache-gc` commands** — hidden commands an
  extension declares in its manifest. `putnami cache clean` and `putnami cache
  gc` fan out to every installed extension that declares them, in name order,
  and report the bytes each reclaimed. Every extension is asked and **every**
  failure is reported: one broken extension no longer leaves the rest silently
  uncollected. `cache gc` also starts each extension's collector opportunistically
  at the end of a run, at most once per extension per hour, detached.

A new ecosystem bounds its own cache by declaring those two commands. No patch to
the CLI is involved, and core exports no per-language cache variable.

## Go compiler and module cache

> Owned by the `@putnami/go` extension.

Go's compiler cache is content-addressed and designed for concurrent processes;
downloaded module versions are immutable. Putnami therefore uses one flat cache
for every repository and worktree on the machine instead of multiplying it by
the number of worktrees:

```text
~/.putnami/cache/go/
├── build/   # GOCACHE: compiled packages for every toolchain/target/content key
├── prog/    # the GOCACHEPROG helper's compiled objects, when a cache provider serves the run
└── mod/     # GOMODCACHE: one copy of every downloaded module version
```

This root is outside the build store (`~/.putnami/store/<repo>`), so the store
budget above neither counts nor evicts it. Only the Go extension's own budget,
below, applies to it.

`./putnamiw` bootstrap builds, Go-extension jobs, and Go-backed extension
launchers all use this same root. In CI, persist `~/.putnami/cache/go`; one
restore then warms bootstrap and every Putnami job on that runner.

Existing per-worktree caches under
`.putnami/extensions/@putnami-go/cache` are not migrated or deleted
automatically because an older Putnami process may still be using them. Once no
older jobs are running, those legacy directories are safe to remove; new jobs
will not repopulate them.

The cache has a separate **10 GiB machine-wide budget**—ten concurrent
worktrees still share one 10 GiB allowance. `putnami cache gc` and the hourly
opportunistic pass after a run account for all three directories, then evict Go
build-cache files oldest-first by the recency timestamps maintained by Go, down
to an 80% low watermark. Neither runs during a run: the store's in-run passes
never touch this root. Entries touched within the grace period are protected. The immutable
module cache is preserved to avoid dependency-download churn and unsafe partial
module deletion; if modules alone exceed the budget, GC reports the excess but
does not delete them automatically.

Because only build entries are evictable, the watermark is applied to the build
cache's share of the budget — the watermark less the module bytes eviction
cannot reclaim — and never drives the build cache below a fifth of the budget.
Once modules alone approach the watermark the target is unreachable, and
deleting every compiled package on the machine would forfeit the whole cache
without bringing the total under budget.

| Variable | Effect | Default |
|----------|--------|---------|
| `PUTNAMI_GO_CACHE_DIR` | Override the shared Go cache root. | `~/.putnami/cache/go`, then `$PUTNAMI_EXTENSION_CACHE_ROOT/go` when no home directory is available |
| `PUTNAMI_GO_CACHE_MAX_BYTES` | Combined compiler/module byte budget across all repos and worktrees. | 10 GiB |
| `PUTNAMI_GO_CACHE_GC_GRACE` | How recently a compiler entry must have been used to be spared. Go duration. Any positive value is widened by one hour before it is applied (see below), so the default spares entries used within ~2h. `0` disables protection. | 1h |

Go refreshes an entry's "last used" mtime only once it is already an hour stale,
so an entry a running compile is reading can present an mtime up to an hour old.
`PUTNAMI_GO_CACHE_GC_GRACE` is therefore widened by that hour before it is
applied — a 5m grace spares entries used within ~65m. Without the margin, GC
could evict a file a concurrent `go build` has already been handed in its
`-importcfg`, which fails that build rather than merely missing the cache. Go's
own trimmer applies the same correction to its age limit.

## Bun package cache

> Owned by the `@putnami/typescript` extension.

Bun already stores downloads once per machine at `~/.bun/install/cache` and
materializes each worktree's `node_modules` with clonefiles on macOS or
hardlinks on Linux. Putnami preserves this native layout: repositories and
worktrees share downloaded bytes, while their mutable dependency trees remain
isolated. In CI, persist `~/.bun/install/cache` and reconstruct `node_modules`
with `bun install --frozen-lockfile`.

A Bun that `@putnami/typescript` installed under the Putnami home keeps its
package cache under its own install,
`~/.putnami/toolchains/bun/bun-<version>/install/cache`, and writes nothing to
`~/.bun`. `putnami cache gc` bounds each of those caches separately, with the
budget and grace period below: each installed release has its own 10 GiB. `PUTNAMI_BUN_CACHE_DIR` or `BUN_INSTALL_CACHE_DIR` names one
directory for every Bun.

Each package cache directory has a separate **10 GiB budget**, enforced by the
extension's `cache-gc` command — run explicitly with `putnami cache gc`, or
started for you at most once an hour in a detached process so directory walks
never sit on a foreground cache-hit path. The collector evicts the oldest
downloaded package, registry-metadata, and cached Bun-binary entries down to an
80% low watermark. Bun does not expose per-package access recency, so this is
intentionally an age-based policy with a 24-hour grace period rather than LRU.
Unknown layouts and Bun's `links/` global virtual store are accounted but
preserved.

`putnami cache clean` does **not** wipe the Bun package cache. It is
re-downloadable but expensive and shared with every other repository on the
machine, so a cold rebuild in one worktree does not cost every other checkout its
downloads; the incremental `ts-types` scratch under `.putnami/cache` is what
clean removes.

TypeScript action-cache keys hash the workspace package/lock files and the root
TypeScript/Biome configuration, so a dependency-only or root-config change cannot
restore stale build, test, or lint results. They also vary with the Bun **binary**
version: Bun produces those outputs, and a lockfile does not move when a user
upgrades Bun in place, so the key carries the ambient `bun --version` alongside
the Go runtime identity.

That version is resolved once per CLI invocation, only when a TypeScript task
needs it, by running `bun --version` under a 5s deadline and a 4KB output cap. A
probe that times out, is killed, exits non-zero, or answers with something that
is not a version line degrades to a reserved identity (`timeout`, `invalid`,
`unavailable`, `unknown`) and logs a warning. A degraded identity is deliberately
distinct from every other one, so a run that could not identify Bun neither
serves nor is served by artifacts a known-good Bun produced — it misses, it never
hits wrongly.

| Variable | Effect | Default |
|----------|--------|---------|
| `PUTNAMI_BUN_CACHE_DIR` | Putnami override for the shared Bun cache root; translated into `BUN_INSTALL_CACHE_DIR` by the TypeScript extension. | Bun's `BUN_INSTALL_CACHE_DIR`, then `~/.bun/install/cache`, then `$PUTNAMI_EXTENSION_CACHE_ROOT/bun` |
| `BUN_INSTALL_CACHE_DIR` | Bun's native cache-root override. | `~/.bun/install/cache` for a Bun of the machine; `install/cache` under its install for a Bun that Putnami installed |
| `PUTNAMI_BUN_CACHE_MAX_BYTES` | Package/metadata byte budget across all repos and worktrees. | 10 GiB |
| `PUTNAMI_BUN_CACHE_GC_GRACE` | How new a downloaded entry must be to be spared. Go duration. | 24h |
| `PUTNAMI_TS_CACHE_GC_GRACE` | How recently a `ts-types` scratch entry under `.putnami/cache` must have been modified to survive `putnami cache gc`. Go duration. | 336h (14 days) |

## Artifact Store (binaries)

Separate from the per-repo build cache above, the CLI keeps a **flat, machine-global, content-addressed store of binaries** — extension/template binaries, the managed Go toolchain, and the downloaded CLI — shared across **every repo and worktree on the machine**. A given `(name, version, os/arch)` artifact is byte-identical everywhere, so unlike the build cache it has **no per-repo sub-level**.

```
~/.putnami/artifacts/
├── sha256/<digest[0:2]>/<digest>/   # extracted extension/template tree; the manifest sits at the dir root
│   ├── lastused                     # recency sidecar for GC
│   └── implementationdigest         # the installed-tree digest of an extension entry, once a run computed it
├── cli/<sha256>/                    # the downloaded prebuilt CLI (content-addressed)
│   ├── putnami                      #   the CLI binary
│   └── lastused                     #   recency sidecar — GC'd by recency, like sha256/ entries
├── cli-source/<source key>/putnami  # the CLI `./putnamiw` built from a source workspace, one per source state
├── roots/<id> → <workspace root>    # GC roots: one per workspace the CLI ran in
├── tmp/                             # staging for atomic, first-writer-wins admits (and GC-demoted remnants)
├── locks/<digest>.lock              # per-digest computation ownership; pruned with its entry
└── .lock                            # advisory lock: admits (shared) vs GC (exclusive)
~/.putnami/toolchains/go/go-<ver>/   # one managed Go toolchain per machine (NOT garbage-collected; see below)
~/.putnami/toolchains/go/go-<ver>.pin.json  # the lock entry go.dev published for that release; a pin reads it and makes no request
```

Extension/template entries under `sha256/` are keyed by the **bare-hex SHA-256 of the download archive**. CLI entries under `cli/` are keyed by the **bare-hex SHA-256 of the executable bytes**; the launcher accepts the current raw registry binary payload and legacy gzip+tar payloads, but always verifies the extracted executable against the digest recorded in `putnami.lock.json` (`cli.integrities[os/arch]`). Each worktree keeps a stable symlink (`.putnami/bin/extensions/<name>`) pointing at the shared digest directory.

The Go-backed `@putnami/typescript`, `@putnami/go`, and `@putnami/python`
extensions declare a runtime executable in their manifests. Installed
extension archives carry that executable at the declared relative path.
Mutable local sources instead declare a `runtime.prepare` command and its
complete input patterns; extension synchronization prepares the executable
before planning and every task directly executes that resolved path. There is
no task-time wrapper, `go run`, toolchain probe, or compatibility fallback.

A local prepared-runtime key covers the extension identity and version, the
complete runtime declaration and ordered input patterns, matched file bytes and
executable modes, the transitive trees of local Go module replacements, the
host OS/architecture, and the runtime ABI. Inputs are sorted before hashing.
A replacement tree is enumerated once and consumed by both the digest and the
staged source view, so the two can never disagree about what a replaced module
may contain. That enumeration excludes VCS/orchestrator state (`.git`,
`.putnami`), the scheduler-owned `.gen` output tree, and the package-manager
install directories the planner already refuses to walk — minus `vendor/` and
`dist/`, which stay because a Go module may vendor its dependencies. Those trees
are derived from a previous task invocation or from a lockfile, so including
them would make the runtime digest follow an install instead of the module's
source, and an install is also what plants the bin symlinks a staged copy cannot
resolve. A symlink inside a replaced module is hashed and staged as a link when
its target stays inside that module; a link whose target is absolute or climbs
out of it is refused by both walks, because the build is content-addressed and
must not read bytes the digest does not cover.
Preparation receives an isolated read-only-shaped source view, runs with
`GOWORK=off`, and writes only below `{runtimeOutput}`. The owner re-hashes all
inputs after preparation and refuses admission if the source changed.

Publication uses the artifact store's exclusive **per-digest** ownership lock
and atomic first-writer-wins admit. Before an executable is admitted or reused,
the CLI verifies that it is a regular non-symlink executable and runs the
bounded `__putnami runtime-info` handshake. That handshake binds extension
name/version, host platform, CLI contract, runtime protocol, and runtime ABI.
A runtime that carries no version of its own, such as the Go and TypeScript
extension runtimes, answers the version of the `putnami.extension.json` that
declares it as its executable, and an empty version when no manifest does.
Its 10-second deadline counts from the moment the operating system has started
the runtime process. The time the operating system spends admitting a freshly
written binary, such as darwin's code-signature assessment on a loaded machine,
is not charged to it. A runtime still running at the deadline fails with
`runtime.handshake_timeout`, which names machine load or a blocked runtime
rather than a malformed build.
Installed archive runtimes go through the same executable and handshake checks
in place. Any prepare, validation, or identity failure stops synchronization;
the CLI never falls back to a legacy wrapper path.

The synchronized content digest is included in local task cache keys, so a
runtime source or replacement-module edit cannot restore a result produced by
older executable bytes. GC/Clean remove orphan digest-lock files only while
holding the store-exclusive lock, avoiding flock inode-split races.

An installed extension's tasks carry the content digest of its installed tree
instead (see [Extension Implementation](#extension-implementation)). An entry
under `sha256/` never changes, so the first run that computes the digest
records it in the entry's `implementationdigest` file, tagged with the digest
schema, and later runs read that file instead of hashing the tree again. The
record is read and written under the store's shared lock, so GC never removes
an entry while its digest is computed or recorded; when the lock is not
available within 2 seconds, the run computes the digest and records nothing. A
record of another schema, or one that cannot be read, is computed again and
replaced. Only an entry inside the store is trusted with a record: a
`node_modules` package or a per-worktree install can change between runs, so
each run hashes it again, and a file named `implementationdigest` there is
payload like any other file. An entry of another store, such as one a link
installed under another `PUTNAMI_ARTIFACT_DIR` names, is hashed again on each
run too, and its record is neither read nor written. The bookkeeping files any
store writes at an entry's root, `lastused` and `implementationdigest`, stay
out of its digest, so the runs that use that store do not move it.

### Zero-init worktrees

Declared extensions and templates are **materialized implicitly from the lock on every command** — a fresh git worktree resolves its toolchain with no explicit `putnami install` and no per-worktree download. On the warm path it is a single stat per artifact; when the lock pins a digest the store already holds (a sibling worktree warmed it), a fresh worktree's "install" is just a symlink swap. A missing or GC-evicted (dangling) link self-heals on the next command. `putnami install` remains the explicit, eager path that resolves `latest`, advances the lock, and runs dependency/context generation.

Resolution order for the artifact store root:

1. `PUTNAMI_ARTIFACT_DIR` — used verbatim (and scopes GC to that single root).
2. `~/.putnami/artifacts` — flat, shared across all repos.
3. `<workspace>/.putnami/artifacts` — last resort when `$HOME` is unavailable (degrades to per-worktree, unshared).

### What stays per-worktree

Only content that is genuinely repo- or branch-specific stays out of the shared store: the `.putnami/cache` build scratch, `.gen/version.json` and other generated files, and `node_modules` (left to the package manager). Installs whose **binary bytes could not be verified** — an `PUTNAMI_UNSAFE_INSTALL=1` install, or a cross-platform lock that has no digest for this OS and whose registry advertised no integrity — also stay per-worktree and are **never** admitted to the shared store, so one repo's unverified bytes can never be served to another.

### Artifact GC

The artifact store has its own collector, separate from the build store's. It **never evicts an entry a workspace links to**, whatever its age: every command registers its workspace under `roots/`, and GC keeps each entry that a registered workspace's `.putnami/bin/putnami` or `.putnami/bin/<kind>/<name>` link resolves to. A root whose workspace no longer exists is pruned, and what it alone kept then ages out normally. Every other entry — whole directories under `sha256/`, and the CLI blobs under `cli/` and `cli-source/` — is evicted by recency: **wall-clock** idle past `PUTNAMI_ARTIFACT_MAX_IDLE`, then oldest-first to get under `PUTNAMI_ARTIFACT_MAX_BYTES`, protected by a grace window and an under-lock recency re-check (a worktree that just linked a binary, or a `./putnamiw` that just touched the CLI blob, is spared). Eviction is **self-healing**: a still-needed extension/template or compiled-extension digest re-materializes on that worktree's next command, and an evicted CLI blob is re-downloaded (or, in a source workspace, rebuilt) by the next `./putnamiw`. The whole pass holds the store's exclusive lock, so it never races an in-flight install or compilation — a genuine lock-acquisition failure aborts the pass rather than deleting unprotected. It runs opportunistically after builds (throttled, non-blocking) and explicitly via `putnami cache gc`; `putnami cache clean --all` clears it.

The managed Go toolchain (`~/.putnami/toolchains`) is a **separate** root and is *not* reclaimed by this GC or by `cache clean` — there is typically one directory per Go minor version and deleting one in use would break a concurrent build. Reclaim it by removing that directory directly (a missing toolchain is re-downloaded on the next build). The pin record beside it, `go-<ver>.pin.json`, is under 1 KiB and is not reclaimed either; after you delete it, the next pin of that release asks go.dev again.

| Variable | Effect | Default |
|----------|--------|---------|
| `PUTNAMI_ARTIFACT_DIR` | Override the store location (and scope GC to it). | `~/.putnami/artifacts` |
| `PUTNAMI_ARTIFACT_MAX_BYTES` | Byte budget for the artifact store (separate from the build store's). | 5 GiB |
| `PUTNAMI_ARTIFACT_GC_GRACE` | How recently an entry must have been used to be spared. Go duration. | 1h |
| `PUTNAMI_ARTIFACT_MAX_IDLE` | Evict entries unused longer than this wall-clock duration, regardless of budget. `0` disables idle reclaim. | 720h (30d) |

## Diagnosing Cache Issues

When results seem stale or caching isn't working as expected:

```bash
# Force re-execution to rule out stale cache
putnami build --no-cache

# Use JSONL to see cache hit/miss per job
putnami build --all --output=jsonl | grep '"cache"'

# Check what would run (shows MISS/NO-CACHE per job)
putnami build --all --plan

# Clean the cache entirely
putnami cache clean
```

### Common Cache Miss Causes

| Cause | Symptom | Fix |
|-------|---------|-----|
| File changed | Different file content hash | Expected behavior |
| Task param changed | Different task params hash | Expected behavior |
| Upstream changed | Different upstream hash | Expected behavior |
| Env var changed | Different env hash | Check `cache.key.env` |
| Wrong glob pattern | Files not included in hash | Update `cache.key.files` |
| Version bump | Different effective project version | Expected behavior |
| Extension changed | Different implementation digest, or a different version for an extension without one | Expected behavior |
| New task format | Format version changed (now `v8`) | Clean cache after CLI upgrade |
