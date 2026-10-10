# Validation

The extension protocol includes comprehensive validation to catch errors before runtime. Validation operates at three levels: schema conformance, structural integrity, and behavioral testing.

## Schema Validation

Every `putnami.extension.json` must conform to the extension JSON Schema. This catches:

- Missing required fields (`commands`, `run`, `task`, `kind`, `command`)
- Invalid field types (e.g., string where boolean expected)
- Unknown fields (the schema uses `additionalProperties: false`)
- Invalid enum values (e.g., invalid visibility, or the deprecated, ignored `cache.restoreMode`, which is still validated against its enum so a manifest that carries it loads)

```bash
putnami dev extension validate [path]
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

### Activation Pattern Validity
`activationFiles` patterns must be valid glob expressions.

### Task Contract (`declares`)

Every rule below fires only on a task that carries a `declares` block, so a
manifest that predates the task contract keeps its exact verdict.

| Rule | Code |
|------|------|
| `kind` is required and is `file` or `directory`; `root` is one of `project`, `workspace`, `command-output` | `required-field`, `invalid-enum` |
| Exactly one of `path` and `pathFrom` is set | `required-field`, `invalid-value` |
| `path` is relative, cleaned, concrete (no glob, no template variable), and never the root itself | `invalid-output-path` |
| `pathFrom` names a port the task declares in its `outputs` map | `unresolved-output-port` |
| `effects` come from the closed vocabulary, without duplicates | `invalid-enum`, `duplicate-effect` |
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
`protocols/cli.GoEmbedInputsContract` (6) for `go-embed:build` or
`go-embed:test` task inputs/cache-key files, or
`protocols/cli.ReleaseBaselineInputContract` (7) for a task that declares the
`releaseBaseline` runtime input. `protocols/cli.GitInputModeContract` names the
same rung 7 for a task whose input port or cache-key files select a `git:`
pattern. The gate applies to every non-empty command, command-group, MCP
tool, or agent-content surface. A non-conforming manifest — for example one
that shadows a reserved global flag — fails packaging and never reaches a
registry. A manifest without the field is contract `0` (pre-registry).

Contracts 5, 6 and 7 are **additive**. They add vocabulary a manifest opts into
and change nothing a manifest without that vocabulary means. An extension without
agent content, Go embed inputs, a `releaseBaseline` input or a `git:` input keeps
its contract-4 stamp; one with agent content but no Go embed inputs requires
contract 5. A manifest with Go embed inputs requires contract 6 even if it also
declares agent content. A manifest with a `releaseBaseline` input or a `git:` input requires
contract 7 whatever else it declares: a reader without the name would key the
task without the baseline, and a reader before 7 keys a `git:` candidate's bytes
without its executable bit and keys an unmerged candidate by its bytes. An
exclusion (`!git:…`) selects nothing and requires no rung. The reader loads
every stamp from the one the manifest's vocabulary requires up to
`protocols/cli.LatestContract` (7):

| Manifest contract vs CLI | Loader behavior |
|--------------------------|-----------------|
| absent (0), or below what its vocabulary requires | **Reject**: "declares CLI contract N but this putnami requires M: re-package the extension … (or run `putnami extensions update` …)". The stamp is the only evidence an extension speaks the contract; a lower one proves nothing about what the manifest relies on. A content manifest stamped 4 names its agent-content contribution as the reason. |
| from the required contract up to the latest | **Enforce**: a reserved shadow is a hard error; the stamp claimed compliance. |
| above the latest | **Reject**: "extension requires a newer putnami (contract N > M)". A future contract is never half-interpreted. |

A manifest that declares **no contract surface** (no commands, command groups,
tools, or agent content) is outside the ladder and loads unstamped: the packager
never stamps one, so requiring a stamp would reject exactly the manifests it
refuses to stamp. `DeclaresContractSurface` is the shared predicate. Agent
content counts as a surface even though it runs nothing: it is exactly what an
older reader would drop without a word, so an unstamped content manifest must
not load as hook-only.

The exemption is checked AFTER the higher-contract arm, not before it: a
hook-only manifest stamped above the latest contract was written against rules
this build does not have, whatever surface it happens to declare today, so it is
rejected like any other future manifest. `TestLoadManifest_CompatibilityMatrix`
(manifest_matrix_test.go) is the executable form of this table — the full cross
product of {absent, one behind, current, latest, one ahead} × {commands,
command groups, tools, agent content, none}. Ratcheting a row means editing
that table and this one together.

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
| current | contract-6 manifest with Go embed inputs | Loads; Go source and embedded input selection is enforced. |
| current | Go embed manifest stamped 5 or lower, or unstamped | Rejected: the stamp does not cover Go embed input semantics. |
| current | agent-content manifest stamped 4, or unstamped | Rejected: the stamp does not cover the contribution. |
| current | contract-3 or lower manifest with a surface | Skipped, with the re-package/`putnami extensions update` remediation on the skip record; a contract-3 reader cannot represent `sessionPrerequisites`, so loading it would permit an ungated dependent command. |
| contract-4 CLI | contract-5 artifact | Rejected by that CLI's own higher-contract arm ("requires a newer putnami"). Its permissive decode would drop `agentContent` silently, which is why the stamp, not the schema, carries the refusal. The remedy is `putnami upgrade`. |
| contract-5 CLI | contract-6 artifact | Rejected by that CLI's own higher-contract arm. It cannot bind Go embed inputs to cache identity. |
| contract-3 CLI | contract-4 artifact | Rejected by that CLI's own higher-contract arm ("requires a newer putnami"). The remedy is `putnami upgrade`; there is no artifact shape that both permits the new field and lets the old reader ignore it. |

Task-contract version stays ORTHOGONAL to this ladder. A contract-4 manifest
whose tasks carry no `declares` block still loads as a task-contract-v2
manifest; requiring every task to declare belongs to the slice that deletes
inferred capture, not to the loader. `FullValidateManifest` and
`putnami extensions validate` remain maximally strict regardless of the declared
contract — they are the authoring surface.
The Go embed file selectors follow one rule at both authoring and load time:
only `go-embed:build` and `go-embed:test` are recognized, and they belong to
project task inputs (or the corresponding project cache-key files). Workspace
and closure inputs cannot carry them. `FullValidateManifest`, the SDK manifest
builder and the negotiated loader all reject the same invalid selector.

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
