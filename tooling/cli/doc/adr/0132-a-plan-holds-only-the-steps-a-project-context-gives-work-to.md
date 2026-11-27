# ADR 0132 — A plan holds only the steps a project's context gives work to

- **Status**: accepted
- **Date**: 2026-11-27
- **Scope**: `@putnami/cli` (`internal/jobs/planner`: step activation;
  `internal/workspace`: the activation witnesses; `cli-model/extension`: the
  pipeline expansion), `go.putnami.dev/protocol/extension` (the activation
  vocabulary, CLI contract 28), `@putnami/go` and `@putnami/typescript` (the
  gates of their pipelines)

## Context

The Go pipeline plans config-merge, tidy, generate and infra for every Go
project, and the TypeScript pipeline generate and infra for every TypeScript
project, whether or not the project has anything for them. On the scale
bench workload (200 projects, `build,test --all`) Putnami plans 1,540
tasks; Nx, Turborepo and moon run about 400 for the same builds and tests.
Every planned task costs something even when the cache serves it: a key, a
cache decision, a session record.

On this repository at `b1b6332c` (`lint,test,build,validate --all`), Go
config-merge has work for 2 of the 131 Go projects that plan it and Go infra
for 32; TypeScript infra for 3 of 20. The pull request that adopted this
record holds the measurement per step.

Extensions already had a plan-time gate, `activation`, evaluated against a
project's files (`files`, `closureFiles`, `contains`, `changedFiles`), all of
which must hold. It could not read the project's type, nor say "a workload,
or a project that holds an output to clear". And each run evaluated every
gate again.

## Decision

1. **The activation vocabulary reads the project's type and alternatives.**
   Two members join the gate, under CLI contract 28 (protocol extension ADR
   0022):
   - `projectTypes`: the project's declared type is listed; `""` names a
     project that declares none.
   - `anyOf`: alternative gates, one of which must hold, besides the gate's
     other conditions.

2. **A gate is decided once per plan, and kept under the witnesses of what it
   read.** The planner memoizes each gate's answer per project for the plan,
   so a gate the build and test pipelines share is decided once. The answer
   is kept in `.putnami/activation-witness.json` with its witnesses, under
   the rule ADR 0113 §2 gives the probe: the stat of each directory
   the gate listed, of each file it read, and of the nearest existing
   directory of each path it found absent. A later run whose stats all hold
   answers the gate with a stat per path, without a walk or a read; one stat
   that differs evaluates the gate again. A witness is kept only when every
   path lies on the workspace's device, no recorded path is a link, none moved during
   the read and lies past the file index's margin before the read began. The
   file names the program that wrote it and its clock, and is written only
   when a run recorded a new answer, never under `--plan` or a preview. The
   key of an answer holds what the files do not: the project, the gate, the
   project's type when the gate reads it, the dependency closure's roots for
   a closure gate, and the workspace's project roots, which a walk does not
   enter.

   ```mermaid
   flowchart LR
     load[workspace load] --> plan[plan: each step gate, per project]
     plan --> memo{answered in this plan?}
     memo -- yes --> use[plan or splice the step]
     memo -- no --> held{witness in activation-witness.json holds?}
     held -- "every stat equal" --> use
     held -- "a stat differs, or none" --> eval[read the files: walk, stop at first match]
     eval --> keep[keep the answer with the stats it read]
     keep --> use
   ```

3. **Each often-idle step states the condition under which it has work.**

   | Step | Has work when | Gate |
   |---|---|---|
   | Go `config-merge` (build, test) | a member of the dependency closure has `conf/.env.yaml` or `conf/.env.*.yaml`; a dependency's merged file comes from its own config | `closureFiles` |
   | Go and TypeScript `infra` | the project is a workload (type `application` or none), or an output an earlier workload build left is present (`.gen/requirements.json`, `.gen/infra/runtime.json`), which the step's run clears | `anyOf` of `projectTypes` and `files` |
   | Go and TypeScript `lint~docs` | a Markdown file lies in the project's tree; the reader checks a README and the documents under a `doc` directory, and any `.md` is the decidable superset | `files: ["**/*.md"]` |

   What an apparently idle step still produces stays planned:
   - **Go `generate`** produces the identity of the project's sources for its
     dependents. Every Go source input of `describe`, `compile` and
     `test-exec` reads only the project's own files; a dependent reaches its
     dependencies' sources through `^generate`, whose key reads them. A
     library whose generate writes nothing but `.gen/generate-result.json`
     still carries that key, and without it an edit to the library would
     leave every dependent's key, and its cached verdict, in place.
   - **TypeScript `generate`** merges `contentHash` into the build stamp
     `.gen/version.json`, which a running application reads through
     `getBuildInfo()`, and writes `.gen/generate-result.json`, which
     `transpile`, `types` and `compile` take through `fromStep: generate`.
     Dropping it changes the artifacts.
   - **Go `tidy`** has work when the module's requirements differ from what
     its imports need. That is the go command's answer over the import graph
     and the module graph; a condition read from `go.mod` would miss the new
     third-party import that is exactly when tidy has work.
   - **The build stamp** `.gen/version.json` is written by the scheduler for
     every planned project, whatever steps it plans. Dropping a step does not
     drop the stamp.
   - **`validate~api`**, `cloud-config-member` and the sdd validations read a
     workspace catalog, the resolved options or the workspace's identities; no
     gate over the project's files decides them, so they stay planned.

   A step whose gate cannot be decided stays planned: a project without a
   resolvable root plans every gated step, as before. On a file system whose
   stats the file index does not trust, no witness is kept and the gate is
   read on every plan.

4. **An inactive step is idle, not unwanted.** `if` and dead-code elimination
   read the manifest's own topology, and the inactive steps are spliced out
   after. Before, they ran on the spliced pipeline: a Go library in a full
   gate excludes `compile` by its condition, and `infra` was the leaf that
   kept `tidy`, `generate` and `describe` live; with `infra` idle they would
   have fallen with it. The plan now differs from the scheduled-and-skipped
   one only by the idle steps.

5. **The Go and TypeScript manifests are stamped 28.** A reader below 28
   refuses the new members, so it refuses the manifest with the remedy
   (`putnami extensions update`) instead of a decoding error.

## Rejected

- **Drop an idle Go `generate`, gated on an application, a config or HTTP
  import, or a file it reconciles.** It removes 86 of 131 generates on this
  repository, but each removed one is a link of the key chain decision 3
  names: an edit to such a library would serve its dependents' cached tests.
  Dropping it needs the dependents' keys to read their closure's sources
  directly, a key change of its own.
- **Gate `infra` with `if: project.type`.** An `if` cannot say "or an output
  is left behind", so a project that stops being a workload would keep the
  requirements an earlier build wrote.
- **Read the gates again on every run.** A recursive gate walks the
  project's tree on every plan, for an answer that moves only when one of the
  directories it listed does.
- **Keep the activation answers in the workspace snapshot.** The snapshot
  is written by the probe, before the plan; its witnesses hold the probe's
  inputs, not the files a gate reads.

## Consequences

- Planned tasks fall on every workload; see the measurements in the pull
  request that adopted this record.
- The key of a task downstream of a dropped config-merge no longer holds the
  dropped step's key, which it held as an upstream hash: such keys move once,
  on the first run with this CLI. The dropped key read only configuration
  files no member of the closure has; the task's own inputs, and the
  artifacts and verdict of every remaining step, are unchanged. Infra and docs
  lint are leaves: no key holds theirs.
- An idle step leaves the files an earlier run of it wrote only where its
  gate names them as a reason to run: infra's gate holds its own two outputs.
- A step that gains work is planned on the next run: the file that gives it
  work changes a stat its witness holds (`TestAStepThatGainsWorkIsPlannedInTheNextRun`).
- An unchanged run reads the witness file and stats its paths; it writes the
  file only when an answer moved.
- A machine-level process can hold the same answers in memory and drop
  one on a file event; the witnesses stay the fallback.
