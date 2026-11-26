# ADR 0015 — The tsc output check reads the candidate cut its key holds

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`): the `build-tsc`
  task and its output check (`internal/build/tsc.go`)
- **Amends**: [ADR 0014](0014-a-typescript-job-reads-the-tree-from-its-context.md)
  point 1, and [ADR 0010](0010-a-tsc-build-declares-its-outdir-through-pathfrom.md)
  on when an output Git does not ignore fails the build
- **Applies**: [protocols/job ADR 0002](../../../../protocols/job/doc/adr/0002-a-job-reads-the-tree-facts-its-key-holds.md)
  and [ADR 0003](../../../../protocols/job/doc/adr/0003-a-job-reads-the-candidate-cut-and-the-change-history-from-its-context.md)

## Context

After ADR 0014, `build-tsc` was the last TypeScript job that started Git: one
`git check-ignore --no-index --stdin` per build, asking whether Git ignores
each output (`outDir`, a `declarationDir` outside it, the `.tsbuildinfo`).
Measured on the TypeScript case of the extension counts test
(`tooling/cli/internal/cli/e2e/orchestration`, `lint,test,build --all` on one
library, every task executed): jobs 1 Git call, the `check-ignore`; engine
25.

The check guards two hazards of ADR 0010. An output that holds a file Git
tracks would lose it when the build empties the output. An output Git does
not ignore enters the build's own key, a `git:**/*` input, so the build keys
the files it emits. Its verdict depended on the ignore rules, which the
build's key does not hold: protocols/job ADR 0002 point 2 refuses a cached
job a fact its key does not cover.

## Decision

1. **`build-tsc` declares `candidateFiles` in place of `trackedFiles`.** The
   candidate cut (protocols/job ADR 0003) is the cut the task's `git:` input
   keys: each path the index tracks, with its mode, and each untracked path
   Git does not ignore. It answers both hazards with the key's own value.
2. **An output is refused when the cut holds a path at or below it.** A
   tracked path (any index mode, an unmerged one included) refuses it as
   `typescript-tsc-tracked-output`; an untracked one as
   `typescript-tsc-output-not-ignored`. Outputs are checked in order, each for
   a tracked path first. The check starts no Git.

   The cut lists a symbolic link, a submodule and a nested repository as one
   entry and nothing below it, so an output below such an entry is refused
   too: as tracked for a tracked link or a submodule, as not ignored for an
   untracked one. Emptying it would delete what Git tracks through the link
   or in the inner repository, where the `check-ignore` of ADR 0014 failed on
   such a path. An output below a symbolic link, one Git ignores included, is
   refused as `typescript-tsc-outdir`, whatever the cut lists: the cut is
   read at the path as written, and the link may lead out of the project or
   to a directory whose tracked files the cut lists under another path. An
   output that is itself a link is removed without being followed.
3. **The second hazard is refused once it keys the build.** An output the
   build has not created holds nothing, so it passes. Once tsc has emitted
   into an output Git does not ignore, the emitted files are in the cut, and
   the first `build-tsc` job whose key reads that cut, which therefore differs,
   refuses the build before tsc runs and names a file. The cut follows the
   run's writes (protocols/job ADR 0003 point 2), so that job may be a later
   tsc step of the same run as well as of the next run. Measured on the
   TypeScript counts workspace with a `.gitignore` that ignores neither `dist`
   nor `*.tsbuildinfo`: a fresh `lint,test,build --all` succeeds, and the next
   one refuses `build~tsc-auto` naming a file it holds
   (`TestATscOutputGitDoesNotIgnoreIsRefusedOnceItKeysTheBuild`).
4. **Git stays where the context cannot answer.** A project inside a
   repository nested in the workspace's (`treefacts.NestedRepository`), whose
   files the workspace's cut does not list, and a context without the cut, an
   older CLI's, are checked as before: the tracked files listed with Git and
   one `check-ignore`, which also tells a directory outside every repository.

Measured after the change on the same case: jobs 0 Git calls, engine 25,
`ls-files` 11 as before. The run already read the cut for `lint-docs`, and
reads it again after each write that can change it when a later key asks
(protocols/job ADR 0003 point 2): the `trackedFiles` read the change removes
is replaced by one such read for the `build-tsc` key. With a second tsc
library in the same workspace, jobs fall from 2 to 0 and the engine stays at
30 (`ls-files` 15): the second library's key adds no read. The cost is
bounded by ADR 0003's: at most one `ls-files` per write a later key follows.
In a run where `build-tsc` is the only task that reads the cut, the reads
are its own: `build --all` on the two libraries went from engine 26 and jobs
2 to engine 29 and jobs 0, one Git call more in all (`ls-files` 12 to 14,
and the once-per-run `check-ignore` of `.putnami` that a write under it
cost once a task read the cut). protocols/job ADR 0004 reads the cut again
only for a key whose directories a write reached, and asks about `.putnami`
only for a key such a write reaches: on the `web` counts case, `build --all`
on an application and its web project, the engine goes from 20 and jobs 1
to engine 21 and jobs 0, the same total.

## Rejected alternatives

- **The engine answers the ignore status of the paths a task declares as
  outputs.** `build-tsc` declares its outputs through `pathFrom` (ADR 0010):
  their paths come from the project's `tsconfig` and are known only once the
  job has read it. The engine cannot answer, before the job, for a path no
  declaration names, and reading `tsconfig` in the CLI is the alternative ADR
  0010 rejected.
- **A tree fact holding the ignore rules, matched in the SDK.** The fact would
  carry every `.gitignore` from the repository root to the project,
  `info/exclude` and the user's `core.excludesFile`, and the SDK would
  reimplement Git's matching. Every key that declares it would move when the
  root `.gitignore` changes (6 of the 873 commits of `main` at `fb51ce42`),
  and the user's global excludes would make keys differ between machines. It
  keeps a first-run refusal that guards no harm the key can see.
- **Keep the `check-ignore` (ADR 0014 point 1).** The job starts one Git per
  build, and its verdict reads rules its key does not hold.
- **`ignoredDirectories`.** ADR 0014 rejected it: it names no file output, and
  the build's own `outDir` enters it after the first build and moves the key.

## Consequences

- A build whose `outDir` or `.tsbuildinfo` Git does not ignore can succeed in
  a fresh checkout, and fails from the first `build-tsc` job whose key holds
  the emitted files, naming one of them. A single CI run on a fresh clone may
  not report it; the files it emits did not key the build that emitted them.
- An output Git ignores that holds a file a negation rule re-includes is now
  refused: that file is in the build's key.
- The build's key moves when an untracked file Git does not ignore appears
  under the project, as its `git:` input already did outside `node_modules`.
- Paths are compared as the cut spells them. On a case-insensitive file
  system, an `outDir` spelled `Dist` while the cut lists `dist/` is not seen
  as holding the files, where `check-ignore` honored `core.ignorecase`.
