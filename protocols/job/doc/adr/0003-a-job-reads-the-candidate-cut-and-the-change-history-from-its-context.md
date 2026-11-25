# ADR 0003 — A job reads the candidate cut and the change history from its context

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/job` (`tree`),
  `go.putnami.dev/protocol/extension` (runtime input names),
  `@putnami/cli` (`internal/jobs`), `go.putnami.dev/sdk/extension`
  (`gitcandidate`, `treefacts`)
- **Amends**: [ADR 0002](0002-a-job-reads-the-tree-facts-its-key-holds.md)
  points 1 and 3, and point 4 for the candidate cut; keeps its points 2, 6 and 7, and `tooling/cli`
  [ADR 0041](../../../../tooling/cli/doc/adr/0041-git-candidate-file-inputs.md),
  [ADR 0059](../../../../tooling/cli/doc/adr/0059-a-cache-key-describes-the-tree-not-the-commit.md),
  [ADR 0060](../../../../tooling/cli/doc/adr/0060-the-base-version-is-not-a-cache-key-input.md) and
  [ADR 0071](../../../../tooling/cli/doc/adr/0071-an-extension-command-calls-the-core-over-a-host-rpc-channel.md)

## Context

ADR 0002 gives a job the paths the index tracks and the directories Git
ignores entirely. The jobs of `@putnami/sdd`, `@putnami/python`,
`@putnami/clientgen` and `@putnami/rust` that still start Git read neither:
they read the candidate cut, the set a `git:` input keys (`tooling/cli` ADR
0041), through `gitcandidate.Open` or the same `ls-files` command.

| Job | Reads | Its key |
|---|---|---|
| sdd `features-validate`, `feature-coverage-validate`, `decisions-validate`, `recipes-validate`, `docs-links-validate`, `codeowners-sync` | the workspace's cut, and the index modes for an evidence fingerprint | `git:**` |
| Python `package-docker` | the cut of the project and of its path dependencies, for files under `dist`, `out` and `vendor` | `git:**/*` of the project and the closure |
| clientgen generated-contract guard | the workspace's cut | `git:**` |
| Rust `package-crate` | the crate root's cut, submodules included, for the staged copy | uncached |
| SDK `docslinks` (Rust `lint-docs`) | the workspace's cut | `git:**` |

The cut is not a function of the two listings of ADR 0002: an untracked file
that a file rule such as `*.pyc` or `.env` ignores is in neither, and a job
that rebuilt the cut from them would ship it in an image or a crate. In the
sdd fixture of `tooling/cli`'s orchestration counts, the jobs make 7
`ls-files` and 2 `rev-parse` calls, all for the cut.

Two sdd steps of `validate-workspace` also read history, both uncached tasks:
the breaking marker reads whether the clone is shallow and the commits
`baseline..HEAD`; the coverage ratchet reads whether the clone is shallow,
`merge-base baseline HEAD` and `putnami.workspace.json` at that commit. The
baseline is the run's impacted baseline (`selection.baseline`).

## Decision

1. **`candidateFiles`, a work-tree fact.** A task that declares the runtime
   input `candidateFiles` receives the cut of the directories its key holds a
   `git:` input of: its own directory, and each member of its dependency
   closure when a closure input holds a `git:` pattern.
   `tree.candidateDirectories` names those directories and
   `tree.candidateFiles` lists the cut at or below them: each entry a path
   relative to the workspace root and its index mode (`100644`, `100755`,
   `120000`, `160000`), `untracked`, or `unmerged`. It is the listing a `git:`
   input enumerates before the disk is read, so a path deleted from the work
   tree is listed and the job drops it as the key does. The key folds the
   listing as it folds `trackedFiles`. The engine reads the workspace's cut
   before the first task starts, with one `ls-files --stage --others
   --exclude-standard`, which lists a tracked path with its mode and an
   untracked one bare. Outside every repository the cut does not exist: the
   key holds that, and the context omits the fact.
2. **A write of the run reads the cut again.** Unlike the facts of ADR 0002
   point 4, the cut follows the run: when a task executes or a restore
   materializes outputs, or a task may have rewritten the sources, the read is
   stale, and the next key or context reads the cut again. A write under the
   workspace's `.putnami` directory leaves the read as it is only when Git's
   rules exclude that directory as a whole and the cut lists nothing below it.
   The engine asks Git that with one `check-ignore`, the first time in a run a
   write under `.putnami` would otherwise need the cut read again, and never
   again in the run; the check is part of the cut read it takes over from the
   jobs. The read is marked stale before the
   memoized input digests the write moves are dropped, so no key computed in
   between keeps a digest of the cut before the write. A key records the read it took, and the
   context of its job carries that read, so a cached job reads the cut its key
   holds. A job whose key took no read, an uncached one, carries the current
   read. A file a dependency of the task generates is therefore in the cut a
   later task reads, as it was when each job listed the cut itself. When the
   workspace root is the top of its work tree, a key's `git:**` digest of the
   whole workspace lists the cut from the same read, so it starts no Git and
   holds what the facts of the run hold. A workspace below the top keeps the
   Git listing, which covers the whole repository (`tooling/cli` ADR 0041), and
   so does a digest bounded by a prefix.
3. **`workspaceCandidateFiles`, the same fact for the workspace.** A project
   task that reads the whole workspace, such as `features-validate`, declares
   `workspaceCandidateFiles` and receives the workspace's cut. A manifest
   whose task declares it without a workspace `git:**` input is refused. The
   listing is written once per run to a file of the run, in the shape of
   `candidateFiles`, and `tree.workspaceCandidates` names that file and the
   SHA-256 of its bytes, which is also the key's value, so 150 tasks do not
   each carry 13,000 paths.
4. **`changeHistory`, a history fact for uncached tasks only.** A task that
   declares `changeHistory` receives, for the run's impacted baseline: the
   baseline, whether the clone is shallow and, when it is not, the merge base
   with `HEAD`, each commit of `baseline..HEAD` newest first with its subject
   and body, and `putnami.workspace.json` as the merge base holds it at the
   workspace's directory. The baseline resolves as the impacted selection
   reads it: a name that is no commit is read as `origin/<baseline>`, for the
   merge base and the commits alike. A manifest whose task declares it while its cache is
   enabled is refused, and the engine writes it to no cached job: a commit is
   state a key never names (ADR 0059, ADR 0060). It adds nothing to a key. A
   run without an impacted baseline writes no `changeHistory`, and the job
   reports what it reports without a baseline. The engine reads it once per
   run, when a job first asks.
5. **An interactive command is not a job.** `contracts check` reads the
   contract source at `HEAD`, and `architecture validate --baseline REF` reads
   a ref its argument names. Both run as interactive commands, outside the
   scheduler, and are no job of the gate: they keep reading Git.
6. **The SDK answers from the context.** `gitcandidate.OpenFrom` takes the
   job's context and answers from `candidateFiles` or
   `workspaceCandidateFiles` when they cover the root asked, and from Git
   otherwise (ADR 0002 point 7). `treefacts` answers the change history from
   the context, and has no Git fallback for it: a job without the member reads
   history as it did before.

## Rejected alternatives

- **Rebuild the cut from `trackedFiles` and `ignoredDirectories`.** A file
  rule hides files from the cut that neither listing names; the image and the
  crate would ship them.
- **Widen `ignoredDirectories` to every ignored path.** It lists every
  ignored file of every build output, larger than the cut it replaces.
- **History in a cached task's key.** ADR 0059 and ADR 0060 keep commits out
  of every key.
- **The workspace cut inline in each context.** 13,000 paths times each
  project's task is the cost ADR 0002 refused.
- **A `headFiles` fact for the contracts check.** The check is an
  interactive command; no planned job would declare it.

## Consequences

- The jobs of the four extensions start no Git when their tasks declare these
  facts, except a crate root outside the project's directory and a crate with
  submodules, whose contents a cut does not list.
- Each write of the run that can change the cut costs one more `ls-files`
  when a later task asks for the cut. A file written outside every declared output
  is not seen until the next write, as for the input digests.
- Declaring `candidateFiles` or `workspaceCandidateFiles` moves a task's key
  when the declaration is added, and then whenever the listing changes: a
  candidate added or removed, and also a change no byte shows, staging an
  untracked file, `git rm --cached` or a mode change in the index. No other
  task's key moves.
- Still read through Git: sdd `verdict-policy-validate`, whose key holds only
  the policy files it names; the clientgen guard's `check-ignore --no-index`,
  on its error path only; a crate root outside its project or with
  submodules; the interactive commands of point 5. The orchestration counts
  of `tooling/cli` exercise none of the Python image, the clientgen guard and
  the Rust `package-crate`. Their unit tests, which run without git on `PATH`,
  prove the helpers that read the cut (`newTreeFilter`, `committedViewFrom`,
  `copyStackRoot`), not the jobs end to end.
- Removing a fact or a runtime input name is breaking.
