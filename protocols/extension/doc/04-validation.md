# Validation

The extension protocol includes comprehensive validation to catch errors before runtime. Validation operates at three levels: schema conformance, structural integrity, and behavioral testing.

## Schema Validation

Every `putnami.extension.json` must conform to the extension JSON Schema. This catches:

- Missing required fields (`commands`, `run`, `task`, `kind`, `command`)
- Invalid field types (e.g., string where boolean expected)
- Unknown fields (the schema uses `additionalProperties: false`)
- Invalid enum values (e.g., invalid visibility, or the deprecated, ignored `cache.restoreMode`, which is still validated against its enum so a manifest that carries it loads)

```bash
putnami extensions validate [path]
```

Without a path, validates the extension in the current directory. With a path, validates the extension at that path.

## Structural Validation

Beyond schema conformance, the validator checks structural integrity:

### Task Reference Resolution
Every `task` field in a pipeline step must reference a task defined in the `tasks` section:

```json
// INVALID: step references nonexistent task
{
  "commands": {
    "build": {
      "run": [{ "id": "build", "task": "nonexistent" }]
    }
  },
  "tasks": {
    "actual-task": { "kind": "command", "command": "echo" }
  }
}
```

### Pipeline DAG Acyclicity
Pipeline step dependencies must form a directed acyclic graph:

```json
// INVALID: circular dependency
{
  "run": [
    { "id": "a", "task": "t1", "dependsOn": ["b"] },
    { "id": "b", "task": "t2", "dependsOn": ["a"] }
  ]
}
```

### Command Dependencies
A command's `dependsOn` entry is `command`, `command~step` or `!command`.
Validation refuses every other spelling with `invalid-command-dependency`: a
`^`, `/` or `*` prefix, an empty command or step, a second `~`, and a session
barrier on one step:

```json
// INVALID: a session barrier names a whole command
{
  "commands": {
    "clientgen": {
      "dependsOn": ["!build~describe"],
      "run": [{ "id": "generate", "task": "generate" }]
    }
  }
}
```

### Contract Schema References
`inputSchemaRef` and `outputSchemaRef` must reference schemas defined in `contracts.schemas`:

```json
// INVALID: references undefined schema
{
  "tasks": {
    "build": {
      "kind": "command",
      "command": "echo",
      "inputSchemaRef": "undefined-schema"
    }
  }
}
```

### Reserved Global Flags
Command, command-group, and subcommand flags must not shadow global CLI flags
from `protocols/cli`, including their short aliases. Names such as `output`,
`json`, `help`, `version`, `verbose`, `debug`, `quiet`, `color`, and aliases
such as `-v` are reserved for the framework CLI contract.

```json
// INVALID: --json and -v are reserved globals
{
  "commands": {
    "test": {
      "flags": {
        "json": { "type": "boolean" },
        "details": { "type": "boolean", "short": "-v" }
      },
      "run": [{ "id": "test", "task": "test-exec" }]
    }
  }
}
```

### Command Names and Step IDs

A plan name joins a command and a step with `~` (`build~compile`), so a command
name and a pipeline step id hold no `~`. Strict validation refuses one with
`invalid-command-name` at `commands.<name>` or `invalid-step-id` at
`commands.<name>.run[<i>].id`, and the schema states both with a pattern. The
load path does not apply the rule.

```json
// INVALID: "a~b" reads as step b of command a, and so does step "x~y"
{
  "commands": {
    "a~b": { "run": [{ "id": "build", "task": "build-exec" }] },
    "build": { "run": [{ "id": "x~y", "task": "build-exec" }] }
  }
}
```

### Activation Pattern Validity
`activationFiles` patterns must be valid glob expressions.

### Command Kinds and Batch Policy

A command's `traits`, a verb's `traits` and a task's `batchable` policy name
their values from closed vocabularies ([Verbs](06-verbs.md#command-kinds)):

| Rule | Code |
|------|------|
| `traits.kind` is `verification`, `criteria`, `diagnostics` or `files` | `invalid-enum` |
| `traits.workload` is `serve` or `once` | `invalid-enum` |
| `batchable.kind` is `verification` or `diagnostics` | `invalid-enum` |
| `batchable.phase` is lowercase letters, digits and hyphens, starting with a letter | `invalid-value` |

```json
// INVALID: "report" is not a command kind, and "Lint" is not a phase name
{
  "commands": {
    "check": { "traits": { "kind": "report" }, "run": [{ "id": "a", "task": "a" }] }
  },
  "tasks": {
    "a": { "kind": "command", "command": "echo", "batchable": { "tool": "lint", "phase": "Lint" } }
  }
}
```

### Host Hooks

The top-level `hostHooks` member declares the hooks an extension asks the agent
hosts to run. Each hook runs one interactive subcommand of the extension, so
the command's exit code reaches the host unchanged.

```json
{
  "hostHooks": {
    "requires": { "contract": "missions" },
    "command": ["agent", "hook"],
    "hosts": {
      "claude-code": [
        { "event": "SessionStart", "args": ["--engine", "claude-code"], "timeoutSeconds": 30 },
        { "event": "PostToolUse", "tools": ["TodoWrite"],
          "args": ["--engine", "claude-code"], "timeoutSeconds": 30 },
        { "event": "Stop", "args": ["--engine", "claude-code", "--idle-wake"], "wake": true, "timeoutSeconds": 660 }
      ],
      "codex": [
        { "event": "Stop", "args": ["--engine", "codex"], "timeoutSeconds": 240 }
      ]
    }
  }
}
```

| Rule | Code |
|------|------|
| The extension declares `agentContent`: the `extension:<name>` opt-in into its content also enables its hooks | `host-hooks-without-agent-content` |
| `requires.contract` names a collaboration contract: lowercase letters, digits and hyphens | `required-field`, `invalid-host-hooks-contract` |
| `command` names a group and an interactive subcommand of this manifest, and any nested subcommand under it | `required-field`, `invalid-host-hooks-command` |
| A host is `claude-code` or `codex`, and lists at least one hook | `unknown-host-hooks-host`, `empty-host-hooks` |
| An event is one its host runs: `SessionStart`, `PostToolUse`, `Stop`, `StopFailure` and `SessionEnd` for Claude Code; `SessionStart`, `PostToolUse` and `Stop` for Codex | `unknown-host-hook-event` |
| Only `PostToolUse` names `tools` (host tool names), and it names at least one; no name repeats | `invalid-host-hook-matcher` |
| Every argument is letters, digits and `_ . / : = -`, so the shell reads it as written | `invalid-host-hook-args` |
| Only a Claude Code hook declares `wake` | `invalid-host-hook-wake` |
| `timeoutSeconds` is from 1 to 3600 | `invalid-host-hook-timeout` |
| No two hooks of a host run the same command at the same point | `duplicate-host-hook` |

### Task Contract (`declares`)

Every rule below fires only on a task that carries a `declares` block, so a
manifest that predates the task contract keeps its exact verdict.

| Rule | Code |
|------|------|
| `kind` is required and is `file` or `directory`; `root` is one of `project`, `workspace`, `command-output` | `required-field`, `invalid-enum` |
| Exactly one of `path` and `pathFrom` is set | `required-field`, `invalid-value` |
| `path` is relative, cleaned, concrete (no glob, no template variable), and never the root itself | `invalid-output-path` |
| `pathFrom` names a port the task declares in its `outputs` map | `unresolved-output-port` |
| `within` is set only with `pathFrom`, and is relative, cleaned, concrete, and never the root itself | `invalid-output-within`, `invalid-output-path` |
| `readByDependents` is set only on a durable output under the `project` root | `invalid-output-read-by-dependents` |
| `effects` come from the closed vocabulary, without duplicates | `invalid-enum`, `duplicate-effect` |
| `publishChannels`, when present, names at least one channel and each channel once | `invalid-value`, `duplicate-publish-channel` |
| A task that declares `publishChannels` also declares the `registry` effect | `effect-conflict` |
| A task cannot both declare outputs and claim `cache.noOutput` | `effect-conflict` |
| A task declaring an external effect (`registry`, `cloud`, `process`) must disable caching — a hit would skip the effect | `effect-conflict` |
| `mutatesSources` and the project-scoped `sources` write resource are declared together or not at all | `effect-conflict` |
| No two declared outputs claim the same path, where nesting and portable case aliases count as the same path | `output-overlap` |

```json
// INVALID: the file lives inside the subtree the other task owns
{
  "tasks": {
    "build-generate": {
      "kind": "command", "command": "echo",
      "declares": { "outputs": { "gen": { "kind": "directory", "path": ".gen" } } }
    },
    "build-describe": {
      "kind": "command", "command": "echo",
      "declares": {
        "outputs": { "openapi": { "kind": "file", "path": ".gen/api/openapi.json" } }
      }
    }
  }
}
```

Overlap between `command-output` paths is scoped to the command whose directory
they live in: two tasks that never appear in the same command write to different
directories and cannot collide, while two steps of one command share it.
Project- and workspace-rooted outputs are compared unconditionally.

`ValidateOutputOwnership` is the manifest-local half of the invariant. The
cross-extension half — two extensions claiming one path in a workspace — belongs
to the planner, which applies the same `OutputsOverlap` predicate to refs whose
roots have been resolved to absolute directories.

## The `cliContract` lifecycle

`cliContract` declares which CLI ↔ extension contract version a manifest was
validated against. It is **earned, not claimed**: the package job (`putnami
package` for Go archive extensions and npm extension packages, and the SDK's
`agentartifact.PackageExtension` for a content-only extension) validates the
staged manifest strictly and stamps `"cliContract": N` on success, where N is
the lowest contract whose vocabulary covers the manifest
(`RequiredCLIContract`): `protocols/cli.CurrentContract` (4),
`protocols/cli.AgentContentContract` (5) for `agentContent`,
`protocols/cli.ReleaseBaselineInputContract` (7) for a task that declares the
`releaseBaseline` runtime input, `protocols/cli.ExtensionDependenciesContract`
(8) for `extensionDependencies` that name a registry extension (`@scope/name`),
`protocols/cli.VerbVocabularyContract` (9) for a `verbs` vocabulary,
`protocols/cli.SourceBoundCacheContract` (10) for a task that declares
`cache.sourceBound`, `protocols/cli.CommandKindContract` (11) for a command
kind, workload or effect, or a batchable kind or phase,
`protocols/cli.PublishChannelsContract` (12) for a task that declares
`declares.publishChannels`,
`protocols/cli.ProvisionedToolchainContract` (13) for a runtime toolchain that
declares a `provisioned` candidate,
`protocols/cli.InputSelectorsContract` (14) for `runtime.inputSelectors` or a
task file pattern that names an input selector,
`protocols/cli.DefaultPositionalsContract` (15) for a command group whose
default subcommand declares positionals,
`protocols/cli.ChangedFilesContract` (16) for a step that gates on the run's
changed files (`activation.changedFiles`) or binds them (`from: context`,
`path: selection.changedFiles`),
`protocols/cli.HostHooksContract` (17) for a `hostHooks` member, or
`protocols/cli.StepDependencyContract` (18) for a command `dependsOn` entry
that names one step of another command (`build~describe`), or
`protocols/cli.ProviderContract` (19) for a `providers` section. Rung 6 named the
`go-embed:build` and `go-embed:test` selectors the CLI expanded itself; rung
14 retires it.
`protocols/cli.GitInputModeContract` names the same rung 7 for a task whose
input port or cache-key files select a `git:` pattern. The gate applies to every non-empty command, command-group,
provider, or agent-content surface, and to a verb vocabulary. A non-conforming manifest — for example one
that shadows a reserved global flag — fails packaging and never reaches a
registry. A manifest without the field is contract `0` (pre-registry).

Contracts 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18 and 19 are **additive**. They add vocabulary a manifest opts into
and change nothing a manifest without that vocabulary means. An extension without
agent content, input selectors, a `releaseBaseline` input or a `git:` input keeps
its contract-4 stamp; one with agent content and nothing else requires
contract 5. A manifest with a `releaseBaseline` input or a `git:` input requires
contract 7 whatever else it declares: a reader without the name would key the
task without the baseline, and a reader before 7 keys a `git:` candidate's bytes
without its executable bit and keys an unmerged candidate by its bytes. An
exclusion (`!git:…`) selects nothing and requires no rung. A manifest whose
`extensionDependencies` names a registry extension requires contract 8 when
the CLI loads it from the artifact store or a declared path, because the CLI
installs and loads those dependencies and an older reader would drop them
(`CheckDependencyContract`). `LoadManifest` enforces `RequiredLoadContract`,
which leaves the dependency rung out; the npm packager stamps that contract. A
manifest that declares `verbs` requires contract 9 wherever it loads from: its
default traits change how the jobs of other extensions are planned, and an
older reader would plan them with other traits. `RequiredLoadContract`
includes that rung. A
manifest with a task that declares `cache.sourceBound` requires contract 10
wherever it loads from: a reader at 10 stores and looks up the task's entry at
a key that also binds the project's source binding, and an older reader would
replay outputs that embed a stale binding.
`RequiredLoadContract` includes that rung too. A manifest whose command or
verb traits declare `kind`, `workload`, `rewritesSources` or
`consumesReleaseSet`, or whose task's `batchable` policy declares `kind` or
`phase`, requires contract 11 wherever it loads from: an older reader would
plan a declared kind or workload as a plain command. `RequiredLoadContract`
includes that rung as well. A manifest with a task that declares
`declares.publishChannels` requires contract 12 wherever it loads from: a
publishing run counts a project's declared channel as served only by a planned
job whose task declares it, and an older reader would judge those channels by
another rule. `RequiredLoadContract` includes that rung too. A manifest whose
runtime toolchain declares a `provisioned` candidate requires contract 13
wherever it loads from: the candidate names no path, and only a reader that
unpacks the pinned archive knows where its executable is. `RequiredLoadContract`
includes that rung as well. A manifest that lists `runtime.inputSelectors` or
whose task file pattern names a selector requires contract 14 wherever it loads
from: a reader at 14 asks the extension for the files each selector reads, and
an older reader expands rung 6's two selectors itself or not at all. A manifest
stamped below 14 that names a selector is refused, whatever its stamp.
`RequiredLoadContract` includes that rung too. A manifest whose command group
defaults to a subcommand that declares positionals requires contract 15
wherever it loads from: a reader at 15 runs that default for a word that names
no subcommand, with the word as its first argument, and an older reader
answers "unknown subcommand". Positionals on a subcommand that is not the
group's default keep the base stamp. `RequiredLoadContract` includes that rung
as well. A manifest with a step that declares `activation.changedFiles` or
binds `selection.changedFiles` from `context` requires contract 16 wherever it
loads from: a reader at 16 plans the step only for a project that owns a
matching changed file and binds those files into the step's cache key, and an
older reader would plan the step on every run and key it without them.
`RequiredLoadContract` includes that rung too. A manifest that declares
`hostHooks` requires contract 17 wherever it loads from: a reader at 17 writes
the declared hooks into the agent hosts' hook files on an explicit install,
init or upgrade, and an older reader would install the extension without them.
`RequiredLoadContract` includes that rung too. A manifest whose command
`dependsOn` names one step of another command (`build~describe`) requires
contract 18 wherever it loads from: a reader at 18 plans the whole command and
makes the dependent's root steps depend on that step and run after the
command's other leaves, and an older reader would read the entry as a command
name and plan nothing. An entry that holds
`~` counts even when it is not well formed. `RequiredLoadContract` includes
that rung too. A manifest that declares `providers` requires contract 19
wherever it loads from: a reader at 19 calls each declared operation through
the provider wire, and an older reader would load the extension with no
provider. `RequiredLoadContract` includes that rung as well. A manifest that
still carries the deleted `tools` section loads leniently with no provider,
and strict validation refuses the member. The reader loads every stamp from
the one the manifest's vocabulary requires up to `protocols/cli.LatestContract` (19):

| Manifest contract vs CLI | Loader behavior |
|--------------------------|-----------------|
| absent (0), or below what its vocabulary requires | **Reject**: "declares CLI contract N but this putnami requires M: re-package the extension … (or run `putnami extensions update` …)". The stamp is the only evidence an extension speaks the contract; a lower one proves nothing about what the manifest relies on. A content manifest stamped 4 names its agent-content contribution as the reason. |
| from the required contract up to the latest | **Enforce**: a reserved shadow is a hard error; the stamp claimed compliance. |
| above the latest | **Reject**: "extension requires a newer putnami (contract N > M)". A future contract is never half-interpreted. |

A manifest that declares **no contract surface** (no commands, command groups,
providers, agent content, or verb vocabulary) is outside the ladder and loads
unstamped: the packager never stamps one, so requiring a stamp would reject
exactly the manifests it refuses to stamp. `DeclaresContractSurface` is the
shared predicate. Agent content and a verb vocabulary count as surfaces even
though they run nothing: each is exactly what an older reader would drop
without a word, so an unstamped content or vocabulary manifest must not load
as hook-only.

The Go archive and npm packagers count a verb vocabulary as surface too. They
validate a manifest that declares one strictly and stamp it 9, a
vocabulary-only manifest included, and a defective vocabulary fails packaging
unstamped, even beside a registry extension dependency. The SDK's
`agentartifact.PackageExtension`, which the scaffold `package` command runs,
stamps 9 on a content-only extension that declares a vocabulary beside its
agent content. It still requires agent content, so it does not package a
vocabulary-only extension.

For the Go archive packager, registry `extensionDependencies` are contract
surface too. A manifest that declares only hooks and such dependencies takes
the stamp-only arm: the packager stamps 8 without the strict validation's
requirement of a command, provider or agent content, and the loader postcondition
still applies. The npm packager leaves the list out of its stamp decision, so
an npm framework package that declares only hooks stays unstamped.

The exemption is checked AFTER the higher-contract arm, not before it: a
hook-only manifest stamped above the latest contract was written against rules
this build does not have, whatever surface it happens to declare today, so it is
rejected like any other future manifest. `TestLoadManifest_CompatibilityMatrix`
(manifest_matrix_test.go) is the executable form of this table — the full cross
product of {absent, one behind, current, latest, one ahead} × {commands,
command groups, providers, agent content, verbs, command kinds, publish
channels, provisioned toolchain, default positionals, changed-files gate,
changed-files binding, host hooks, step dependency, none}. Ratcheting a row means editing that table and
this one together.

### The packaging ratchet

The gate turns the stamp in one direction only:

| Staged manifest declares | Packager behavior |
|--------------------------|-------------------|
| nothing, or a **lower** contract | **Stamp at the required contract.** The stamp is earned by the validation the package job just ran, so an author's stale `2` is replaced rather than obeyed; requiring authors to hand-edit a field the packager owns would guarantee drift. |
| **equal** | **Re-stamp** (idempotent, byte-identical). |
| **higher** | **Fail packaging**: "declares CLI contract N but this packager implements M and cannot certify it: upgrade putnami". A packager cannot run the checks of a contract it does not implement, and silently writing its own lower number would publish an artifact asserting a compliance nobody verified — which the loader, seeing its own number, would then accept. |

Both arms of the gate — stamped and hook-only-exempt — end with the same
postcondition: **the staged manifest LOADS**, checked through `LoadManifest`
itself. Since contract 3 there is no tolerant arm, so "validated" and "loadable"
are no longer the same statement, and only the second one has a consumer on the
other end of it. This is what stops a hook-only manifest with a malformed field
the contract does not govern (say `"hooks": []`) from shipping: it would parse as
raw JSON, skip the gate, and then fail to unmarshal in every consumer's CLI,
which drops the whole extension. It is also what stops a Go or npm package from
shipping `agentContent` before its packager can build and stamp it: such a
manifest never reaches contract 5 there, so the loader postcondition fails the
package job.

### Cross-version expectations

| CLI | Artifact | Outcome |
|-----|----------|---------|
| current | contract-4 manifest without agent content | Loads; reserved-shadow and same-session prerequisite rules enforced. |
| current | contract-5 manifest | Loads; its agent content is materialized only in a workspace that opts in. |
| current | contract-14 manifest with input selectors | Loads; the CLI asks the extension for the files each selector reads. |
| current | manifest naming an input selector, stamped 13 or lower, or unstamped | Rejected for its input selectors, with `putnami extensions update` as the remedy: the stamp does not cover the input-resolve call. |
| current | contract-15 manifest whose group default declares positionals | Loads; a word after the group that names no subcommand runs the default with the word as its first argument. |
| current | manifest whose group default declares positionals, stamped 14 or lower, or unstamped | Rejected for its default subcommand that takes positionals, with `putnami extensions update` as the remedy. |
| current | contract-16 manifest whose step gates on or binds the changed files | Loads; the step is planned only for a project that owns a matching changed file, and the files key it. A run that measured no diff plans no gated step. |
| current | manifest whose step gates on or binds the changed files, stamped 15 or lower, or unstamped | Rejected for a step that reads the run's changed files, with `putnami extensions update` as the remedy. |
| current | contract-16 manifest with an input binding the planner does not resolve: `from: command`, or a context path other than `selection.changedFiles` | Rejected for that binding. Strict validation refuses it at any stamp; a manifest stamped 15 or lower loads, and the binding binds nothing. |
| current | contract-18 manifest whose command depends on `build~describe` | Loads; the planner plans all of `build` and the dependent's root steps wait for `describe` alone. A step the plan does not hold adds no edge. |
| current | manifest whose command `dependsOn` holds `~`, stamped 17 or lower, or unstamped | Rejected for a dependency on one step of another command, with `putnami extensions update` as the remedy. |
| current | contract-18 manifest with a command dependency that is not `command`, `command~step` or `!command` | Rejected for that entry. Strict validation refuses it at any stamp. |
| current | agent-content manifest stamped 4, or unstamped | Rejected: the stamp does not cover the contribution. |
| current | manifest naming a registry dependency, stamped 7 or lower or unstamped, from the artifact store or a declared path | Rejected: the stamp does not cover the dependencies the CLI would install and load. |
| current | contract-3 or lower manifest with a surface | Skipped, with the re-package/`putnami extensions update` remediation on the skip record; a contract-3 reader cannot represent `sessionPrerequisites`, so loading it would permit an ungated dependent command. |
| contract-4 CLI | contract-5 artifact | Rejected by that CLI's own higher-contract arm ("requires a newer putnami"). Its permissive decode would drop `agentContent` silently, which is why the stamp, not the schema, carries the refusal. The remedy is `putnami upgrade`. |
| contract-13 CLI | contract-14 artifact | Rejected by that CLI's own higher-contract arm. It expands no selector through the extension. |
| contract-14 CLI | contract-15 artifact | Rejected by that CLI's own higher-contract arm. It would answer "unknown subcommand" for a word the default takes. |
| contract-15 CLI | contract-16 artifact | Rejected by that CLI's own higher-contract arm. It would plan a change-gated step on every run and key it without the changed files. |
| contract-17 CLI | contract-18 artifact | Rejected by that CLI's own higher-contract arm. It would read `build~describe` as a command name, plan nothing and run the dependent before the step it reads. |
| contract-3 CLI | contract-4 artifact | Rejected by that CLI's own higher-contract arm ("requires a newer putnami"). The remedy is `putnami upgrade`; there is no artifact shape that both permits the new field and lets the old reader ignore it. |

Task-contract version stays ORTHOGONAL to this ladder. A contract-4 manifest
whose tasks carry no `declares` block still loads as a task-contract-v2
manifest; requiring every task to declare belongs to the slice that deletes
inferred capture, not to the loader. `FullValidateManifest` and
`putnami extensions validate` remain maximally strict regardless of the declared
contract — they are the authoring surface.
Input selectors follow one rule at both authoring and load time: a task file
pattern whose scheme is not `git` names a selector, which must be one the same
manifest lists in `runtime.inputSelectors`, as an include of a project task
input (or of the project cache-key files). Workspace and closure inputs cannot
carry one, and neither can an exclusion. `FullValidateManifest`, the SDK
manifest builder and the negotiated loader all reject the same invalid
selector; the loader checks the stamp first, so an old build is refused with
its remedy.

## Conformance Fixtures

The protocol includes conformance fixtures for testing validators:

```
fixtures/
├── valid/        # Manifests that must pass validation
│   ├── minimal.json
│   ├── full.json
│   └── task-contract-v3.json
└── invalid/      # Manifests that must fail validation
    ├── missing-commands.json
    ├── empty-commands.json
    ├── missing-run.json
    ├── missing-task-ref.json
    ├── reserved-global-flag.json
    ├── output-overlap.json
    ├── effect-conflict-cached-registry.json
    ├── source-mutation-unserialized.json
    ├── no-output-with-declared-outputs.json
    └── declared-output-escapes-root.json
```

Each task-contract fixture certifies one rule: `TestV3InvalidFixtures` pins the
exact diagnostic codes it must produce, so a fixture cannot pass by failing for
an unrelated reason.

## Behavioral Testing

Beyond static validation, extensions should be tested against fixture projects:

```bash
putnami extensions test [path]
```

This runs each command against a minimal fixture project and verifies:
- The command produces valid JSONL output
- A `result` event is emitted with a valid status
- Declared output artifacts exist after execution
- The exit code matches the reported status
