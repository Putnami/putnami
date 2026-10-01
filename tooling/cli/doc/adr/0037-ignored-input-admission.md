# ADR 0037 — A required ignored input is bound or the request is refused

- **Status**: accepted
- **Scope**: `tooling/cli/internal/runnerprovider` (admission),
  `internal/jobs` (`PortableInputs`), `internal/store` (key collectors),
  the runner source manifest

## Context

The portable source snapshot ([ADR 0032](0032-portable-runner-foundation.md))
captures tracked and non-ignored untracked bytes. A task whose cache key
selects a git-ignored file that exists locally (a `conf/env.local.yaml`, a
`.env`, an undeclared generated fixture) would execute remotely without it,
and the remote verdict would differ from the local one for the same request.
A task keyed on an environment variable has the same defect: no snapshot
carries the value.

## Decision

### Admission reads the plan's own declarations

Admission runs in the engine's portable seam, after the unsupported-shape
refusal and before anything is captured or transferred, on the final planned
jobs. For every job it resolves the file set the job's cache key hashes (key
files, the job's and the command's `filePatterns`, project option layers,
closure patterns, workspace patterns, generate assets) through the store's own
collectors (`store.CollectKeyFiles`, `store.ExtraKeyFiles`). It cannot disagree
with the key about which files a task requires. `jobs.PortableInputs` is that
projection, memoized per (root, pattern set); `runnerprovider.AdmitInputs`
decides on it.

Only an explicit declaration binds. A declared key or workspace file, a closure
pattern, a `filePatterns` entry at any layer, or a generate asset is the task
saying what it reads, so an ignored file it selects is a required input. A task
with no declaration keys on the whole non-hidden project tree; that is the
key's fallback, not a declaration. Nothing selected only by the fallback is
bound: binding it would ship a laptop's `dist/` or `__pycache__/`, or refuse
the request over one stale oversized binary. It is not silent either. Before
submission the run prints one diagnostic per such task, naming up to five
ignored files ("and N more"), saying they are hashed into the local key but do
not travel, and that declaring them as `filePatterns`, or as a task output,
binds them. The run continues.

Git decides which paths are ignored (`git check-ignore -z --stdin` from the
workspace root). A tracked path is never ignored, whatever the rules say: the
question is "would a fresh clone lack this file?". Each ignored input is then
classified:

- **Recreated: not bound.** A path with a component named `.gen`,
  `node_modules` or `.putnami` at any depth, or under a declared output of any
  planned task (a `declares.outputs` literal path or a legacy cache `outputs`
  path). The executing engine recreates it through the same lifecycle.
- **Required: bound.** Anything else. A bound path passes the manifest's path
  rules, is a regular file or a symlink, is not under `.git`, and fits the
  protocol's file-size limit. Otherwise the request is refused with a
  diagnostic naming the task and the path. A directory is never bound.

A cacheable task whose key reads environment variables (`env` inputs or the
project's `envInputs`) is REFUSED before submission, with one diagnostic
naming the task key and the variable names. Values never travel: the request
stays credential-free and machine-free. The refusal is a usage error like an
unsupported shape. `--where local` and the no-provider fallback still run the
task. Runtime inputs need nothing: the pinned environment carries toolchain
identity.

### Binding is on the wire, deterministic, and verified twice

- A bound manifest entry carries `bound: true`, present only as true; an
  explicit `false` is rejected. It is part of the canonical bytes, so binding
  a path changes the source digest.
- The request's `source.bound` lists the admitted paths (sorted, unique, absent
  when empty), and the execution-input digest covers it.
- Capture takes the admitted paths as a parameter. A bound path joins both
  scans and the race check, must still be ignored by Git at capture time (a
  bound path Git does not ignore is an error, never a silent demotion), and is
  exempt from the ignored-tree exclusion for that exact path only. The client
  refuses a snapshot whose bound entries differ from the admitted list.
- The executing engine has no Git index, so it re-derives admission from the
  plan's declarations on the tree it received (`runnerprovider.VerifyAdmission`,
  beside `validateExpectedPlan`). Every bound path must be selected by an
  explicit declaration of a planned task, sit outside every recreated root, and
  exist as a file or symlink; no planned task may key on environment. A
  difference refuses the run before anything is scheduled.

### Limits

- Submodules are rejected at capture.
- Required inputs outside the workspace root cannot be captured and are not
  bound.
- Provider-side blob authorization and retention belong to the provider
  adapter, not to this admission.

## Consequences

- The store owns the only definition of "what a key sees": admission adds two
  pure projections of the existing collection and no second collector.
- A developer who relies on an undeclared ignored file gets a diagnostic, not a
  divergent remote verdict. Declaring the file fixes it.
