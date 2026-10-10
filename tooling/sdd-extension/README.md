# @putnami/sdd

Specification-driven development — features, specs, architecture and contracts —
as a standalone Putnami extension.

**Status: extracted.** This project owns the whole SDD surface: the four
engines, the two validation jobs the DAG and CI run, the four interactive
command groups, and the five `sdd.*` MCP tools. The CLI has no copy —
`putnami features|specs|architecture|contracts` do not exist in a workspace that
does not declare this extension.

Support status: **experimental** — no compatibility promise yet. See
[`putnami.support.json`](../../putnami.support.json).

## Documentation

| Page | What it covers |
|---|---|
| [doc/01-getting-started.md](doc/01-getting-started.md) | Declaring the extension, per-project activation, first run |
| [doc/02-validation-jobs.md](doc/02-validation-jobs.md) | `validate` and `validate-workspace`, the caching policy, the wire |
| [doc/03-commands.md](doc/03-commands.md) | The sixteen interactive subcommands |
| [doc/04-mcp-tools.md](doc/04-mcp-tools.md) | The five `sdd.*` tools, architecture context, and the breaking rename |
| [doc/05-parity.md](doc/05-parity.md) | How parity with the replaced surface is proven |

The decisions behind all of it are in
[ADR 0013 — SDD is a standalone extension](../cli/doc/adr/0013-sdd-as-a-standalone-extension.md),
[ADR 0007 — the spec surface](../cli/doc/adr/0007-read-mostly-spec-surface.md), and
[ADR 0008 — the architecture boundary](../cli/doc/adr/0008-executable-architecture-command-boundary.md).
This README stays the implementer's map of the tree; the pages above are the
user's.

## Why it is an extension

SDD is an optional part of the Putnami DX, and it was compiled into the CLI. As
an extension it costs nothing in a workspace that does not use it, its
validation joins the job DAG the standard way — manifest-declared tasks, no
planner special case — and its semantics stay where they belong, in
`protocols/architecture` and `protocols/spec`.

## Layout

| Path | What it is |
|------|------------|
| `putnami.extension.json` | The manifest. Authored by the extension SDK's builder and pinned by `manifest_contract_test.go`. |
| `cmd/putnami-sdd/` | The single executable every task and subcommand runs. `main.go` answers the runtime handshake, then routes: one leading argument is a job, two are a command path. |
| `bin/prepare` | Builds the executable into the prepared-runtime directory the CLI hands it. |
| `manifest_contract_test.go` | The manifest's conformance harness: the protocol's own validation, plus the one-author rule. |
| `internal/sdd/` | The four SDD engines: what a features, specs, architecture or contracts run DECIDES. Copied from `tooling/cli/internal/commands/sdd`, minus its command and rendering layer. |
| `internal/features/` | The feature and spec engine: discovery, aggregation, evidence freshness, snapshot deltas. Copied from `tooling/cli/internal/features`; only its imports differ. |
| `internal/gitread/` | Immutable Git history reads over the `git` binary, for the revision reader and for source bindings. Copied from `tooling/cli/internal/git`, narrowed to what the engine calls. |
| `internal/wsview/` | The extension's read-only view of workspace membership, and the adapter that builds it from the job context. |
| `cmd/putnami-sdd/validate.go` | The job bodies `validate` and `validate-workspace` run. Each builds the view its wire context entitles it to, calls ONE engine builder, and publishes the diagnostics. |
| `cmd/putnami-sdd/interactive.go` | The sixteen interactive subcommands: argument arity, selection refusals, output-mode resolution, and the result envelope. |
| `cmd/putnami-sdd/render_*.go`, `labels.go` | The human rendering, ported verbatim from the CLI's command layer. |
| `manifest_groups_test.go` | The four command groups, authored — and the assertions that hold the manifest to the inventory table. |
| `cmd/putnami-sdd/mcptool.go` | The five `sdd.*` MCP tools: one request on stdin, one result on stdout, through the SDK's `mcp.Serve`. |
| `manifest_tools_test.go` | The five tools, authored — the names, closed schemas, read-only contract, and `workspaceSelection` declaration. |
| `doc/` | The user-facing pages listed above. |

"Copied from" above is provenance, not a live twin: the CLI paths it names were
deleted with the vertical, and this package is now the only implementation. The
one exception is `internal/gitread`, which core still has a narrower version of
for its own callers — `ResolveCommit`, `HeadSHA`, `ResolveBaselineDetailed` and
`ProjectSourceBindingMeasured`. Those four must stay byte-compatible with the
copies here, because a source binding computed on one side is compared with one
recorded on the other. The record one worktree path contributes to a source
binding is shared instead of copied: both sides read it through the extension
SDK's `sourcebinding` package.
| `putnami.features.json`, `specs/` | This extension's own authored feature and its spec — it validates itself, through the same jobs it contributes. |

## Engine, not command

`internal/sdd` stops at `Build<Something>Result`: it returns the report a run
produced and the classified error the run failed with, and it never writes a
byte to stdout. Argument parsing, positional arity, human output and the
ResultV2 envelope live in `cmd/putnami-sdd`.

The split is why the CLI and the MCP tools cannot answer differently — they call
one builder — and it is also why the engines take a workspace value rather than
a path: an extension has no loader, so membership arrives on the wire and is
passed in.

## The interactive surface

Four groups, sixteen subcommands, all `interactive: true`:

| Group | Subcommands | Positionals | Selection flags |
|---|---|---|---|
| `features` | `list` `validate` `snapshot` `inspect` `diff` | `[query]`, none, none, `<feature-id>`, `<base> <head>` | honored, honored, honored, **refused**, **refused** |
| `specs` | `list` `validate` `verify` `baseline` `inspect` `init` | none, none, none, none, `<feature-id>`, `<feature-id>` | honored, honored, honored, **refused**, **refused**, **refused** |
| `architecture` | `validate` `snapshot` `inspect` | none, none, `<domain-id>` | global `--baseline` |
| `contracts` | `generate` `check` | none (`--project <selector>`) | `--projects` |

`interactive` is the one deliberate scheduler bypass, and every one of
the sixteen needs it: the CLI spawns the process with the terminal's own stdout
and parses nothing, so what the binary writes IS the command's output. A
subcommand that lost the flag would be planned instead, the live renderer would
own stdout, and the result envelope would be swallowed as job chatter — silently.

Three things about that path are easy to get wrong, and all three are pinned by
tests:

- **The output mode is a param, not a flag.** `--output` is a reserved global a
  manifest may not declare and the CLI consumes before dispatch, so the resolved
  mode arrives as `params["output"]`.
- **Selection arrives resolved.** The CLI resolves `--projects/--impacted/…`
  against the workspace it loaded and publishes the answer as `selection`.
  Nothing here re-resolves it. Three raw flags cannot be read back out of a
  resolved answer — `--projects`' original spelling, `--all`, and a standalone
  `--baseline` — and the CLI forwards exactly those three as params.
- **Error texts are contract.** Positional-arity messages and the
  selection refusal are verbatim from the handlers they replace; a paraphrase
  breaks a script that matched on them.

### Parity, measured

The extraction's acceptance is that `@putnami/sdd` writes the bytes the built-in
command wrote. While core still had its implementation it was measured against a
LIVE ORACLE: `tooling/cli/internal/cli/sdd_extraction_parity_test.go` ran every
subcommand twice over one fixture workspace — once through
`App.runStructuredCommand`, once through the real interactive extension path
with a freshly compiled binary — and compared stdout, stderr and the exit code.
Byte for byte, with no normalization.

Removing core removes the oracle, so those answers were RECORDED from core
first, at the parent of the removal commit: 74 fixtures under
`tooling/cli/internal/cli/testdata/sdd-parity`, with no `-update` flag, because
the only implementation that could rewrite them is the one under test. Full
account in [doc/05-parity.md](doc/05-parity.md).

One adaptation was needed to reach byte parity, and it is not cosmetic. A
built-in structured command's payload is CAPTURED as JSON and decoded into
`map[string]any` before the CLI re-encodes it, so its keys come out
alphabetical; a Go struct encodes in declaration order. `capturedPayload`
reproduces that round trip for a SUCCESS envelope. A FAILURE envelope must not
be round-tripped — core passes the attached value straight through — and the
asymmetry is core's, not a choice made here.

### What parity does not cover

Three differences are the CLI's, not this extension's, and cannot be reproduced
from inside a subprocess the CLI never reaches:

- `putnami features nonexistent` is answered by the CLI's extension dispatcher
  before this binary runs, so the message is its ("unknown subcommand", with the
  subcommands listed alphabetically) and the exit code is 1 rather than the
  built-in's 2.
- `putnami features` with no subcommand prints the group's subcommand listing
  instead of the built-in's "unknown subcommand" error.
- An undeclared flag is a deprecation WARNING for an extension group and a hard
  rejection for a built-in.

One more is narrower: a selection-rejecting subcommand given an INVALID selector
(`putnami features inspect X --projects nope`) now reports the selector's own
failure, because the CLI resolves the selection before dispatch. A valid
selector still gets the verbatim refusal.

## The agent surface

Five MCP tools expose bounded, read-only context to an agent. Four are the
feature/spec questions extracted from core; the fifth projects the existing
ARC/DARC evaluation for one exact domain:

| Tool | Replaces | Arguments |
|---|---|---|
| `sdd.list_features` | `list_features` | `query`, `projects`, `impacted`, `baseline`, `cursor`, `limit` |
| `sdd.feature_context` | `feature_context` | `feature` |
| `sdd.list_specs` | `list_specs` | `projects`, `impacted`, `baseline` |
| `sdd.spec_context` | `spec_context` | `feature` |
| `sdd.architecture_context` | New agent projection of `architecture inspect` | `domain` |

The names are dot-namespaced because they have to be: an extension tool name
without a dot is dropped by the CLI's extension-tool validator, and a name core
already owns loses the collision to core. D4 renames all four with **no alias**,
so agent configuration and docs move in the same PR.

`cmd/putnami-sdd/mcptool.go` is the whole implementation: `mcp-tool` on argv
routes one `ToolCallRequest` from stdin to one of five adapters over the SAME
engine builders the commands call, and writes one `ToolCallResult`. There is no
MCP server here — the CLI owns JSON-RPC and the agent's connection.

### The wire had to grow, and what it grew

A tool call carries no job context, and until this task it carried no workspace
either: `ToolCallRequest` had a `workspaceRoot` and nothing else. That is not a
detail an extension can work around, because the MUST NOT list items #2 and #3 forbid
both remedies — no re-derivation by scanning, no copied loader — and for good
reason: resolving `impacted` means diffing a baseline, mapping changed paths to
owners and walking the dependent graph, and a second implementation of that is a
second definition of what the workspace contains.

So the contract grew additively, the same way selfcheck and `135550670` grew the
job wire. A tool declares `"workspaceSelection": true`, and the CLI publishes:

| The engine wants | The request member that carries it |
|---|---|
| Every workspace project, with its resolved version, type and direct edges | `workspaceProjects` |
| A SIBLING's authored `putnami.json` (`bin`, `featureAuthority`, `options`) | `workspaceProjects[].config`, raw |
| What the caller asked for and what it resolved to | `selection`, byte-identical to the job wire's |

`workspaceProjects[].config` is shared with the job wire: both surfaces answer
workspace questions and therefore may need a sibling's authored facts. The tool
also receives resolved authored members such as tags directly. `feature_context`
reads a project's `bin` to mint its command nodes; without the raw config it
would report a project with no commands, and say so confidently.

The declaration is opt-in rather than implicit because `projects`, `impacted`
and `baseline` are the CLI's vocabulary and not every extension's: a third-party
tool that means something else by `projects` must not have its arguments
rejected as unknown project selectors.

### Measured, not assumed

Before the wire carried any of it, `sdd.list_features` over this repository's own
parity fixture returned:

```json
{ "selection": { "mode": "all", "scoped": false, "projects": [] },
  "manifests": 0, "features": [] }
```

Exit zero, well-formed, and empty — over a workspace with two authored features
and a spec. That is the same shape the validation jobs caught (`valid: true, specs: 0`), and
it is why every handler here REFUSES a request with no resolved `selection`
instead of falling back to the whole workspace. A wrong answer an agent cannot
tell from a right one is worse than an error.

Parity is measured the way the interactive surface's was:
`tooling/cli/internal/cli/sdd_mcp_parity_test.go` runs twenty calls — every
tool, every narrowing its schema declares, and every failure — through ONE real
`mcp.Server` carrying the four extracted tools registered by real discovery,
and compares their content blocks byte for byte against the answers recorded
from core before they were removed. The `sdd.list_features` recordings hold
the bounded page this tool answers instead, and change only with the contract
in [doc/04-mcp-tools.md](doc/04-mcp-tools.md). `sdd.architecture_context` postdates that
oracle; its acceptance instead pins the shared evaluator, worktree-only
boundary, complete typed projection, and failure envelope in this project.

Two adaptations were needed and neither is cosmetic:

- **No `capturedPayload` here.** The interactive path re-encodes its payload
  through `map[string]any` because the CLI captures a built-in command's stdout,
  so its keys come out alphabetical. `handleToolsCall` does the opposite — it
  hands the handler's return value straight to `json.MarshalIndent` — so a
  report struct encodes in declaration order and applying the interactive
  adaptation here would BREAK parity rather than achieve it. Same extraction,
  opposite rule, one wire apart.
- **A failure may carry its report.** A core tool that fails with a value
  attached writes two content blocks, the value then the message; the SDK's
  `mcp.Serve` collapsed both into the message alone. It now reproduces core's
  split, and each handler mirrors its counterpart's: argument guards return a nil
  payload, engine calls return the report beside the error.

## The manifest has one author

The committed `putnami.extension.json` must be exactly the document
`authoredManifest` in `manifest_contract_test.go` produces through
`go.putnami.dev/sdk/extension/manifest`. Edit the Go authoring code and the
JSON together; the test compares both in the protocol's canonical form and
fails on any difference.

Two members are set outside the builder, and the test says so where it does it:
`runtime` (the SDK builder has no method for the runtime lifecycle primitive
yet) and `cliContract`.

## The runtime is prepared, not shipped

`runtime.prepare` runs `bin/prepare`, which compiles `./cmd/putnami-sdd` into
`compiled/putnami-sdd` under the directory the CLI passes as `--output`. Two
details of that script are load-bearing:

- It builds to `compiled/putnami-sdd.tmp.$$` and `mv`s the result into place.
  Overwriting a running binary in place fails with `ETXTBSY` on macOS; `rename(2)`
  is atomic and does not. Do not simplify it to a direct `go build -o`.
- It runs with `GOWORK=off` and a scrubbed environment, so the artifact depends
  on this module's own `go.mod` and nothing ambient.

The prepare declaration lists `bin/prepare` among its own `inputs`. The artifact
digest is taken over that list, so without it an edit to the build script would
leave the previously built binary in place.

## Running it by hand

```bash
# The identity handshake core uses after preparing the runtime.
go run ./cmd/putnami-sdd __putnami runtime-info

# What the extension sees on the job-context wire, through the CLI.
putnami sdd-selfcheck

# The five tools, through the CLI's own MCP server.
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | putnami mcp
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sdd.list_specs","arguments":{"projects":["@putnami/cli"]}}}' | putnami mcp
```

`sdd-selfcheck` is an internal command. It reports the extension identity, the
job-context protocol version and the resolved `selection` block — the member
every later SDD verdict depends on, because a validator told it ran over three
projects cannot otherwise tell a deliberate `--projects` narrowing from a
whole-workspace run of a three-project tree.

## The validation jobs

Two commands, because their SUBJECTS differ and one command carries one
activation:

| Command | Scope | Steps | Cached? |
|---|---|---|---|
| `validate` | one project, activated by `putnami.features.json` or `specs/*.json` | `features-validate` → `specs-validate` | features: **no**; specs: yes |
| `validate-workspace` | the workspace, once | `architecture-validate`, `specs-ratchet-validate`, `decisions-validate`, `recipes-validate`, `codeowners-sync`, `docs-links-validate` | architecture and ratchet: yes; decisions, recipes, CODEOWNERS and docs links: **no** |

Both are in the canonical gate:

```bash
putnami lint,test,build,validate --impacted --enforce-coverage
```

The automatic architecture step resolves only the committed workspace
`options.sdd.verification.architecture`: `enforce` blocks on coherent findings,
`report` preserves the typed report and emits its findings as warnings without
blocking, and `off` skips evaluation while reporting mode and provenance.
Structural/config/provider-view errors remain blocking. This repository pins
`enforce`; the shared default for a workspace with no policy is `report`.

A project is validated only when it **declares this extension**. Activation is
two conditions — the command's `activationFiles` must match, and the project's
`extensions` array must name the provider — and the second is the workspace's
opt-in, which is what lets an unused workspace pay nothing. It is also the one
silent way to escape the gate, so `workspace_adoption_test.go` asserts that
every project in THIS repository which authors a feature manifest or a spec
declares `/tooling/sdd-extension`.

### Why one task is uncached and the others are not

A task may be cacheable only when its declared inputs COVER what it reads.

- `specs-validate` REPORTS ON one project and READS the workspace's authored
  identities, because a spec is keyed by a feature id and "one spec per feature"
  is a workspace-wide guarantee. The key follows the read set, not the
  ownership: two project ports name what the task reports on, and a workspace
  port (`**/putnami.features.json`, `**/specs/*.json`) names the identity tier
  it resolves against. A key that stopped at the project would let a sibling's
  manifest change this verdict silently.
- `architecture-validate` reads the workspace's ARC declarations, capability
  evidence, debt artifacts, and `putnami.workspace.json` for its adoption
  policy. The workspace file is a declared cache input, so a mode change cannot
  restore a verdict keyed under another policy.
- `specs-ratchet-validate` keys on the manifests, specs, options and every
  committed `specs.baseline.json` — both sides of its shrink-only comparison
  are committed files its patterns name.
- `decisions-validate`, `recipes-validate`, `codeowners-sync` and
  `docs-links-validate` read files no narrow pattern names: the globs a
  repository authors in its `decisions.json`, the sample directories an index
  names, every `putnami.json` above a project, and any file a link names. Each
  reads the repository's Git candidate cut and nothing else: the tracked files
  and the untracked files no ignore rule excludes. Each declares the input
  `git:**`, which holds exactly that cut, so editing, adding, deleting or
  renaming a candidate moves the key, and an ignored file is neither read nor
  keyed. Outside a Git work tree the steps read the disk, and the `git:` input
  has no key, so they run every time.
- `features-validate` is **uncached**. An evidence source binding records each
  bound file's executable bit and each submodule's checked-out commit, which a
  `git:` input does not hold. Package-root evidence is matched against the
  project's version, which the CLI gives a cacheable task as `0.0.0`. The report
  names the HEAD commit, which a replayed entry would report for another commit.
  An under-declared key does not merely miss a change — it serves a stale
  verdict while claiming to have checked. Keying it needs the executable bit and
  the submodule commit in the `git:` digest, a report without HEAD, and a rule
  for the version.

### The decision gate

`decisions-validate` proves every committed `decisions.json` against this
worktree: the one at the workspace root, for decisions every project follows,
and the one in each project directory, for that project's own. A project
registry's globs and `adr` link are relative to its directory, so it judges
only its own files. Ids are unique across all registries.

Each entry states a settled value with a stable id, the date it was settled and
who settled it, and carries either a `json-value` check or the `reviewOnly`
mark. A violation fails naming the decision — id, statement, settled date and
registry — so the way out is to change the decision, not the code:

```
decision D-001 "serverless workloads scale to zero when idle" is violated by sites/putnami.dev/infra/requirements.json: scaling.minInstances = 1
Settled 2026-09-03 by fdumay. To change it, change the decision in decisions.json, not the code.
```

An absent registry is adoption, never a failure; an unusable one fails closed
rather than enforcing the entries that happened to parse. There is **no
verification-mode knob**: `architecture` has one because a workspace adopts a
graph gradually, while a decision a repository wrote down and dated is either
held or re-decided. The wire and the check semantics live in
`protocols/features`; see
[ADR 0030](../cli/doc/adr/0030-settled-decisions-are-a-registry-validate-reads.md).

### The recipe gate

`recipes-validate` reads every committed `typescript/samples/recipes.json`,
`go/samples/recipes.json` and `python/samples/recipes.json`. Each recipe maps
one intention to the sample that demonstrates it, the framework primitives it
uses, the hand-rolled shapes it replaces, and the version it was recorded at.
A recipe whose `sample` is not a directory in this worktree fails with
`sdd.recipe_sample_missing`, naming the sample; a second recipe for one
intention, an unknown member, or a field that is empty or longer than one line
fails with `sdd.invalid_recipe_index`. An absent index is adoption. A sample
directory exists when it holds a tracked or unignored file, and the task is
cached on the input `git:**`, so emptying, renaming or deleting a sample
directory moves the key. `putnami context generate` renders the indexes
into the `putnami-plan` skill's `references/recipes.md`.

### CODEOWNERS

`codeowners-sync` writes `.github/CODEOWNERS` from the owners each
`putnami.json` declares, next to the code they own:

| File | `options.sdd.owners` gives |
|---|---|
| `putnami.workspace.json` | The catch-all rule, `* <owners>` |
| A scope or project `putnami.json` | A rule for its directory, `/<path>/ <owners>` |

```json
{ "name": "@acme/billing", "options": { "sdd": { "owners": ["@acme/billing-team"] } } }
```

An owner is `@user`, `@org/team`, or an email address. A directory rule comes
after the rules of the directories above it, and CODEOWNERS applies the last
matching rule, so a project that declares nothing belongs to the nearest
declaration above it. A workspace with one maintainer declares the catch-all
only and gets `* @maintainer`. When the workspace commits a `specs.baseline.json`,
the files the spec gate reads (`**/specs.baseline.json`, `**/specs/*.json`,
`**/putnami.features.json` and `putnami.workspace.json`) close the file with the
workspace owners, so they keep that review inside a team's directory.

The step rewrites the committed file only when it differs, so the file follows
the declarations without a command of its own. On CI, the rule that fails a
run whose gate changed the tree reports a stale or missing committed file. An
invalid owner fails with `sdd.invalid_owners`, and so do owners in a root
`putnami.json`, which belong in `putnami.workspace.json`. A directory
declaration without a catch-all fails with `sdd.codeowners_no_default`. A
`putnami.json` the step cannot read or parse fails with
`sdd.codeowners_read_failed`, and an `options.sdd` that is not an object fails
with `sdd.invalid_owners`, whether or not the workspace declares owners: either
file may hide a declaration. No failure writes the file. A workspace that
declares no owners is not adopted, and its `CODEOWNERS`, if any, stays as
written. The task reads the Git candidate cut and is cached on the input
`git:**`, which also holds the file it rewrites. The CLI never replays a run
that rewrote `CODEOWNERS`: the next run is keyed on the new bytes, and only a
run that left the tree unchanged is replayed.

### The job path never reads git history

`architecture-validate` evaluates the current worktree and does not compare
against the frozen adoption baseline. The comparison resolves a commit — which
ref `origin/HEAD` names, what the branch forked from — and none of that is in
the task's key, so a restored verdict would depend on state the key does not
name. Dropping it makes the check **stricter**, never laxer: nothing is excused
as known debt. Shrink-only ratcheting stays with the interactive
`putnami architecture validate --baseline REF`.

Interactive architecture validate/snapshot/inspect remain explicit requests
and are unchanged when the automatic mode is `off`.

## Boundaries

The extension binary may not import `go.putnami.dev/tooling/cli/internal/...`,
may not `replace` into `tooling/cli`, and may not exec `putnami` at run time.
Everything it knows about the workspace arrives on a wire: the job-context
document for a job or an interactive subcommand, the `ToolCallRequest` for an
MCP tool.

That boundary costs something, and `internal/wsview` is where you see it. The
CLI engines read a workspace the loader resolved: every project, with its
version, its edges and its manifest. Each shortfall is a gap in `protocols/job`
to close, never a fact to re-derive here — two derivations of the same project
identity is how a validator and the orchestrator start disagreeing about what
the workspace contains.

Five are closed:

| The engine wants | The wire member that carries it |
|---|---|
| Both path forms of every selected project | `selectedProjects`, written by the planned AND the interactive path |
| Each project's resolved version | `ProjectRef.version` — a package-root source selector keys on name **and** version |
| Every workspace project | `workspaceProjects`, the complete membership, on every job's document |
| Resolved direct dependency edges | `ProjectRef.dependencies`, published inside `workspaceProjects` |
| The job's own `publish` / `options` | `project.publish` and `project.options`, already on every document |

The last three were closed by measurement, not by inspection. Given only
`selectedProjects`, `architecture validate` reported `observedEdges: 0` over a
workspace with one undeclared cross-domain dependency (a false pass), and a
narrowed run reported `architecture.unknown_project` for a real member (a false
violation). Given no membership at all, a project-scoped spec verdict read zero
documents and returned `valid: true` over a project whose manifest and spec were
both broken — and given ONLY its own project, it reported
`go/templates/go-library`'s real parent relation, authored in `tooling/scaffold`,
as dangling. `internal/sdd/{architecture,specs}_wire_test.go` pin all of them as
verdict-parity assertions against a loader-built view.

One limitation remains on the JOB wire and fails closed rather than quietly:

| The engine wants | Consequence today |
|---|---|
| Loader warning codes | The view raises `workspace.provider_view_unavailable` itself when a workspace-scoped job receives no membership, so `architecture validate` refuses instead of reporting a subset as the whole. |

The tool wire's own members are listed under
[The wire had to grow](#the-wire-had-to-grow-and-what-it-grew), and the same
rule governs them: a fact the request does not carry is a gap in
`protocols/extension` to close, never a fact to re-derive here.
