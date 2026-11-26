# ADR 0019 — A delegated verb runs as one tool command

- **Status**: accepted; amended on 2026-11-26: decision
  `delegate-runs-one-process-per-task` is renamed
  `a-delegated-verb-runs-as-one-tool-command`, moves to
  `tooling/stacks/decisions/`, and no longer lists the command of each tool
- **Date**: 2026-11-24
- **Scope**: `tooling/stacks` (`@putnami/stacks`), `@putnami/cli` (the
  scheduler's batching), `integrations/moon` (`@putnami/moon`),
  `integrations/nx` (`@putnami/nx`), `integrations/turbo` (`@putnami/turbo`),
  `integrations/rush` (`@putnami/rush`), `integrations/pants`
  (`@putnami/pants`) and `integrations/bazel` (`@putnami/bazel`)
- **Amends**: [ADR 0008](0008-a-delegated-stack-keeps-its-tool-cache.md),
  which ran the tool once per component, and
  [ADR 0097 of tooling/cli](../../../cli/doc/adr/0097-a-project-names-what-a-command-waits-for.md),
  which kept one tool process per delegated task

## Context

A delegated task starts its own tool process on its own project (ADR 0008, the
decision then named `delegate-runs-one-process-per-task`), and the tasks of one stack root hold
the root's lock, so they also run one after the other. On a warm cache, the
tool alone runs the same work several times faster. Measured on 2026-11-11
against the canary of that day, on the second run of an unchanged tree:

| Repository | Tool alone, one call | Putnami delegate, run 2 |
| --- | --- | --- |
| moonrepo/moon, 10 tasks | `moon run`: 0.2 s | 4.8 s |
| ngrx, 6 tasks | `nx run-many`: 1.5 s | 9.4 s, tasks end 1.4 s apart |
| TanStack/query, 15 builds | `nx affected -t build`: 2.0 s | 25.8 s |
| rushstack, 4 tasks | `rush test --impacted-by`: 0.85 s | 9.3 s |
| st2, 28 tasks | `pants … lint package`: 2.3 s | 182.6 s |

The scheduler already runs a group of tasks as one process under the batch
contract, one result per project id, but only for cached tasks, and a
delegated task has its cache off.

## Decision

- A task of a delegated profile may state `batch.steps`: the steps that run
  the task once over several components. In a batch step, an argument that
  holds `{component}` is repeated once per component, in the order of the
  selection, and `{components}` is the components joined with commas. moon
  states `moon run {component}:<task>`, which runs `moon run a:lint b:lint`;
  Nx states `nx run-many -t <task> -p {components}`; turbo `turbo run <task>
  --filter={component} --summarize`; Rush `rush lint --only {component}`,
  `rush test --only {component}` and `rush build --to {component}`; Pants
  `pants <goal> {component}:`; Bazel `bazel test` or `bazel build` over
  `//{component}:all`.
- A batch step may name `{batch}`, a directory the core creates empty for
  each command and removes once every project read its reports: Bazel writes
  its build-event file there. A batch may state its own `reportSteps`, which
  run once per project after the command, whatever its outcome, with that
  project's `{component}` and `{output}`, `{root}`, `{runtime}` and
  `{batch}`: Bazel collects each package's JUnit and coverage there. A task
  with report steps batches only when its batch states its own.
- The manifest declares such a task `batchable`, with the stack id as its
  tool, and keeps its cache off. The conformance kit holds the manifest to the
  profile: a task is batchable exactly when its profile task states
  `batch.steps`.
- The scheduler batches a batchable task whose cache is off. It groups the
  ready tasks that share a batch key, with no project cap unless the policy
  states one, and a group waits for its peers: while a task of the same
  extension, task and tool is not ready yet, and depends on no task of that
  verb, the ready ones wait, as long as other work is in flight. The graph
  steps of a delegated stack end at different times, so without the wait one
  verb would run one command per wave of graph steps. When no work is in
  flight, the ready tasks run at once. The batch key leaves out the project's
  own option layers of the stack's extension: the stack reads each member's
  from the project, and a sync writes them per project.
- The shared invocation runs one command per stack root, batch shape and
  resolved toolchain: for each, it runs the toolchain check once, takes the
  stack root's lock once, removes each component's reports and the tool's own
  report files the stack names, and runs the batch
  steps at the root. It then answers one result per selected project id. The
  core hands the stack the command's report after every command, and counts
  the outcomes only when the command failed: a stack may remove there a file
  its command wrote. The stack extension reads the tool's report:
  - moon: the run report moon writes in `.moon/cache/runReport.json` during
    the command. A target moon reports `passed` or `cached` passes; a target
    it reports failed fails with moon's error and the end of the output moon
    kept for that task.
  - Nx: the static output of `nx run-many`. Each target's section starts at
    `> nx run <project>:<target>`, and the closing summary lists the failed
    targets. A failed target fails with its section; a target with a section
    and not listed passes.
  - turbo: the run summary `--summarize` writes under `.turbo/runs`: the
    newest one written since the command started whose command runs the same
    tasks with the same filters, whatever their form. A task `<package>#<task>` whose execution exited 0
    passes; one that exited otherwise, or carries an error, fails with
    turbo's error. A task turbo stopped or never started is not listed. The
    reader removes the summary it read.
  - Rush: the summary Rush prints at the end of the command, one list per
    status. An operation of the project, named `<project>` or `<project>
    (<phase>)`, that Rush lists as succeeded, restored from cache, already up
    to date or with no work passes; one listed as failed, or with warnings,
    which fail the command, fails with the details Rush printed for it. A
    project listed as blocked or aborted is not named.
  - Pants: what `pants test --test-show-rerun-command` prints, one line per
    test target, `<sigil> <address> succeeded|failed …`, then the command
    that reruns every target whose test failed, those Pants prints no line
    for included. A package the rerun command or a failed line names fails
    with them; one whose own lines all succeeded passes. A line that stands
    for a batch of files passes no package. An output with no rerun command,
    or one that names a target of no package of the command, names no
    package. `pants lint` and `pants package` report per tool, not per
    package: their failure names no package.
  - Bazel: the build-event file the command wrote in `{batch}`. A package's
    targets are the labels `//<package>:<name>`. A package fails when one of
    its tests ends other than passed or flaky, or one of its targets failed or
    was aborted, with Bazel's status and the end of the test's log; it passes
    when one of its targets completed, none failed and, for `bazel test`, each
    of its test targets has a summary that passed. A failure of a label of no
    selected package, as a test a suite of another package runs, and a file
    Bazel did not finish, with no last message, name no package.
  - A target the report does not name, when the command fails, fails as not
    run, naming the targets that failed. A command that fails and whose
    report names no failed target fails every project with the step's
    message: the report cannot say which one failed. A command that passes
    passes every project.
- Each project then reads its own JUnit and coverage reports into its own
  output directory, as one process per task reads them, and its result
  records `delegatedTo`. A report file that several projects' patterns
  match, as Pants's `dist/test/reports`, is read by the project the stack
  names as its owner: Pants names each JUnit file after the address of its
  test target, as `Address.path_safe_spec` writes it, such as
  `src.a.x_test.py.tests.xml` for `src/a/x_test.py:tests` and
  `src.a.sub.x_test.py@tests.xml` for `src/a/sub/x_test.py:../tests`, a file
  of a nested directory that the target of `src/a` holds; a generated name
  follows after `@` and parameters after `@@`. The Pants stack
  owns a file by the address of the command's output whose name it carries,
  from a table it builds once per command,
  so a nested package never reads its parent's report; a name no address
  matches goes to the one package that prefixes it. A file the stack names
  no owner of, as one coverage file of the whole command, is read by every
  project, which counts the sources it owns.
- A verb stops as a whole. Within one command, the tool decides whether it
  runs the targets left after a failure. Putnami's fail-fast acts between
  commands: a failed project fails the verb's command, and the run stops the
  other commands it would have started.
- A task that runs alone, such as a retry or the only task of its verb, runs
  its own steps, as before.
- A project's override that replaces a task's `steps` drops the profile's
  `batch` unless it states its own: the batch stands for the steps it
  replaced. A sync that writes a project's steps writes their batch beside
  them: moon writes `moon run {component}:<its task>`, so `moon run api:test
  pricing:unit` runs both in one command, and Nx one `nx run-many -t <target>`
  per mapped target. Members whose batch steps differ beyond the argument
  that names their component run separate commands.

## Rejected alternatives

- **Batch only the peers that are ready together, as cached tasks do.** Each
  delegated task waits for its own project's graph step, which waits for the
  graph steps upstream; the tasks become ready in waves, and one verb would
  start one tool command per wave.
- **A fixed coalescing window.** It idles the run by a fixed delay even when
  every peer is ready, and still splits a verb whose graph steps take longer.
- **`nx affected` or `moon run --affected`.** The selection stays Putnami's:
  impact, `--projects` and the import list choose the projects.
- **One command for every verb of a run, such as `moon run a:lint a:test`.**
  The order between verbs, and the fail-fast between them, are Putnami's.
- **Read each project's result from its exit code.** One command has one exit
  code; only the tool's report says which project failed.

## Consequences

- A component that passed a step which failed before the last one fails as
  not run: the later steps ran on no component.
- One command per stack root, batch shape and toolchain, including the
  version each member's own version files state: members that differ run
  separate commands.
- The scheduler reads a batchable task whose cache is off as a delegated
  tool's: any extension that declares one gets the wait for its peers.
- A serve in flight does not count as work that ends: a verb never waits on
  it.
- The command line grows with the number of projects of the verb; nothing
  bounds it yet, so a verb over very many projects may exceed the limit of
  the platform, Windows' 32,767 characters first.
- A warm delegated `putnami lint` on moon or Nx starts one tool process per
  verb, plus one per later wave of a verb whose projects depend on each other
  through a `^command` entry ([ADR 0097 of tooling/cli](../../../cli/doc/adr/0097-a-project-names-what-a-command-waits-for.md)).
- Within one command, a failing project stops the other targets at the
  tool's own rule, and a target the tool does not run fails as not run.
- A run report or an output the tool changes its format of fails every
  project of a failing command with the step's message, never passes one.
- The scheduler groups only tasks whose other parameters match, such as the
  workspace's options and the command's flags.
- A workspace synced before this change fails `putnami validate` with the
  stack's stale-sync diagnostic until `putnami projects sync` writes the batch
  beside the steps it wrote.
- A failed `pants lint` or `pants package` fails every package of its
  command: Pants's output does not say which package failed.
- Pants names the JUnit report of a pytest batch, the targets one
  `batch_compatibility_tag` groups, `batch-of-<address>+<n>-files.xml`: no
  package owns it, so every package of the command reads its cases. When a
  batch spans packages, their test counts include each other's cases, and a
  failed case fails each of them.
- Bazel's report step reads the whole build-event file once per package.
- A Rush project with warnings fails when the command failed, even under a policy that
  allows warnings: the summary does not say whether they failed it.
- turbo writes a run summary under `.turbo/runs` for each command, and the
  reader removes it; `--summarize` needs turbo 1.9 or later.
- Bazel's build-event file of a batch lives in a directory the core removes:
  the file of a task that runs alone stays in its output directory.
