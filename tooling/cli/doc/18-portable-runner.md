# Portable runner

Ordinary job commands accept `--where local|remote`. Omission means local.
When no extension declares `runner-provider`, either placement executes the
ordinary local engine once and returns its exit code. This needs no execution
provider installation, authentication or source upload. A session records both
the requested placement and the actual placement.

```sh
./putnamiw lint,test,build,validate --projects my-app --where local
./putnamiw lint,test,build,validate --projects my-app --where remote
```

## With a provider

When exactly one extension declares `runner-provider`, `--where remote` runs
the ordinary engine up to and including the final plan and the production
preflight, then leaves the engine through one seam:

1. The run is projected onto the strict `protocols/runner` execution request:
   the ordered commands, every command parameter with its Go type, the
   effective execution flags (including whether `--no-cache` was typed), the
   frozen selection (requested and effective mode, canonical project ids, the
   baseline resolved to a commit, changed paths, selection notices,
   `--no-cache-projects` ids), the expected plan (task identities, dependency
   and serialization edges, task-contract digests, deadlines, declared
   resources; never cache presence), and the pinned environment (CLI source or
   version, extensions, toolchains, platform).
2. The plan's declared inputs are admitted: every git-ignored file a planned
   task's cache key selects is bound into the request, and a cacheable task
   keyed on environment variables is refused (see "Required inputs" below).
3. The worktree is captured into an immutable snapshot (tracked and non-ignored
   untracked bytes, including partially staged edits, plus the bound files
   flagged `bound`) and bound into the request with its Git context and the
   HEAD-bound tree fingerprint. A snapshot whose bound entries differ from
   the admitted list is never submitted.
4. The provider process is started, the protocol and capabilities are
   negotiated, only the source blobs the provider lacks are transferred (a
   second snapshot with one edited file transfers one blob), and the bound
   request is submitted. Acceptance is not completion.
5. The executing engine's output records are forwarded unchanged: stdout lines
   to stdout, stderr lines to stderr. A `--output=json` run therefore prints
   the remote result document exactly as a local run would.
6. The canonical session bundle is fetched and imported atomically into
   `.putnami/sessions`, nested sessions and their parent links included, then
   linked as `latest`. The local exit code is the remote gate's.

An empty impacted selection is still the explicit local no-op it always was
and submits nothing. A provider that is installed but cannot initialize, that
speaks another protocol version, or that lacks the execution-request
capability is a precise error; it is never a reason to run locally, and no
task runs in the submitting worktree. Two providers are an ambiguous
configuration. An edit made after submission never reaches execution.

Refused before submission, with a diagnostic: `--watch`, `serve`, `run`,
`format`, an executing `--dry-run`, tasks with registry or cloud side effects,
tasks whose declared cwd leaves the workspace, and cacheable tasks keyed on
environment variables. Verification never publishes or deploys through this
path.

## Required inputs

A fresh clone lacks what Git ignores, and so does the snapshot — unless a
planned task declares that it needs it. For every planned task the engine
resolves the files its cache key hashes through an explicit declaration (its
declared key files and `filePatterns`, the project's
`options.<cmd>.filePatterns`, closure and workspace patterns, generate
assets) through the same collector the key uses, asks Git which of them are
ignored, and binds each one into the request and the snapshot with its
manifest entry flagged `bound: true`. The source digest moves with a binding
exactly as it moves with content. A local run is unaffected.

A task that declares no file input keys on its whole non-hidden project
tree. That fallback is not a declaration of what the task reads, so nothing
it selects is bound — binding it would ship a build directory or refuse the
request over one stale artifact past the file-size limit. Instead the run
prints, before submission, one warning per such task naming it and up to
five of the ignored files its key hashes that do not travel, with the
remedy: declare them as `filePatterns`, or as a task output when the task
generates them. The run continues.

Not bound, because the executing engine recreates them through its own
lifecycle: paths under a `.gen`, `node_modules` or `.putnami` component at
any depth, and paths under an output a planned task declares. A task whose
required input is absent locally runs in the snapshot without it and fails
there the way it would in a fresh clone; nothing is bound silently. A bound
path must be a regular file or a symlink that Git ignores; anything else is
refused naming the task and the path, and a bound file edited between the
two capture scans is a capture race.

A cacheable task whose key reads environment variables (`env` inputs,
`envInputs`) is refused before submission, naming the task and the
variables: values never travel with a request. `--where local` and the
no-provider fallback still run it.

The executing engine re-derives the admission on the materialized tree —
every admitted path must be a declared input of a planned task, outside the
recreated roots, present as a file or symlink, and no task may key on
environment — and refuses a divergence before scheduling, exactly as it
refuses a divergent plan.

## The executing side

A provider launches the snapshot's pinned entrypoint — `./putnamiw` when the
lock records `cli.source: workspace`, so changed CLI source is built from the
snapshot and never replaced by a baked binary — with no arguments and
`PUTNAMI_RUNNER_REQUEST` naming the bound request. The engine consumes that
variable before anything else runs and strips it from every task environment,
so a task that calls the CLI again (a nested session, a hook, an extension
callback through `PUTNAMI_CLI_EXECUTABLE`) runs as an ordinary invocation and
is never hijacked into a second bound execution. That engine restores
workspace state through the ordinary first-use bootstrap, plans exactly the
frozen project ids while keeping the recorded selection mode, and compares
its plan against the expected one before scheduling: any difference in
identities, edges, contracts, deadlines, cacheability or declared resources
refuses the run and records no session. A request without
`invocation.publication` is refused earlier, on the first plan, when that plan
holds a `publish` or `deploy` task, a task that declares registry or cloud
effects, or a release-set publication its tagged versions start: no
release-set or deploy provider starts for it. The session of an executed
request states `placement: {requested: remote, actual: remote}`, the submitter's tree
fingerprint and branch, and the frozen baseline.

## Durable attempts, resume and cancellation

Every submission is recorded under `.putnami/runner/attempts/<key>.json`
after the provider negotiated the request and before anything is sent, so a
failed negotiation leaves no record. The record is keyed by the request's
idempotency key and holds the provider, the execution-input and source
digests, the attempt reference once acknowledged, the last output cursor
forwarded, and the terminal outcome (state, exit code, imported session, or
the retrieval or import error verbatim). The record is
the transport's ledger; the verdict stays the imported session.

Before a run submits, it resolves any submission still in flight for the same
inputs through the same provider and asks the provider to `lookup` the key. A
hit is resumed from the persisted cursor; a miss is submitted under the same
key. A CLI that died mid-run, a lost acknowledgement and a duplicated submit
envelope therefore all resolve to one attempt. A settled attempt never blocks:
running the same gate again after it finished is an intentional retry and
gets a new attempt. The submitting side also refuses to adopt an attempt that
executed different inputs.

`putnami sessions inspect --run <ref>` takes the attempt reference or the
submission key printed at submission. An attempt whose session is imported is
shown from the store; one that is not is resumed through the workspace's
runner provider from the persisted cursor and imported through the same path.
Nothing is ever resubmitted from there. `putnami sessions list --revision
<sha>` lists the sessions whose recorded tree sat on that commit (a lowercase
hex prefix, 7 to 64 characters; never a ref name) with the head commit and the
actual placement, so a new session can find what judged a revision.

Output is followed with an ordered cursor persisted as it arrives. A transport
loss reconnects a bounded number of times and replays from the cursor; a
record at or below it is dropped, so a line is never printed twice. Ctrl-C
sends `cancel`, waits a bounded time for the acknowledgement and then for the
terminal state, reports what it observed and exits with the signal code
(130); the provider terminates the attempt's process tree. A cancel that
raced a completion keeps the provider's outcome: the completed attempt is
imported and the exit code is the remote gate's.

Expired artifacts, a bundle whose bytes do not match their digests and an
import the validator refuses are explicit failures: the record keeps the
remote state and exit code beside the error, no local session is written,
nothing is rerun, and `sessions inspect --run <ref>` retries the import once
the provider serves the bundle again.

The executing engine's session states `placement.provenance` — the source
digest, the execution-input digest and the submission key of the bound
request it ran — and the importer compares all three members with the
submission it resolved, before anything is published, so a session that
answers another submission never reaches the store or `latest`.

## Import rules

Every bundled file is verified against its digest and size. `session.json`
and `plan.json` must validate against the result-v2 contract and name the
session that carries them; the run report, coverage report and spec
verification record must name it too. A session id that already exists
locally with identical content is a safe repeat; different content is a
collision and nothing is written. A completed attempt that recorded no
session keeps its non-zero exit code, and a provider claiming success without
a session is refused.

## Limits of this slice

The Cloud adapter, trusted cache reuse across placements, change association
and the CI migration are later slices. Submodules and sparse or unmerged
indexes are still rejected at capture. Provider-side blob authorization and
retention belong to the Cloud adapter. A required input outside the
workspace root cannot be captured and is not bound. In-run source rewrites
(a lint fixer) apply to the snapshot only.

The existing session tree fingerprint stays HEAD-bound. The source digest
describes content independently of HEAD; it does not replace task cache keys
or prove that a later clean commit may reuse a dirty run's entries. Pinned
runtime, platform, branch, SHA and dirty-state cache rules continue to apply.

See [the portable execution decision](adr/0032-portable-runner-foundation.md)
and [the input admission decision](adr/0037-ignored-input-admission.md).
