# Getting started with `@putnami/sdd`

`@putnami/sdd` adds specification-driven development to a Putnami workspace:
authored features, durable specs, executable architecture, and generated
contracts. It is an optional extension. A workspace that does not declare it
pays nothing and never sees the commands.

Support status: **experimental**. No compatibility promise yet — see
[`putnami.support.json`](../../../putnami.support.json).

## Declare it

Add the extension to `putnami.workspace.json`:

```json
{
  "extensions": ["@putnami/sdd"]
}
```

Then install and regenerate the agent context:

```bash
putnami extensions install
putnami context generate
```

In a workspace that develops the extension itself, name it by project id
(`/tooling/sdd-extension`) instead: a workspace-local extension is attached by
project id, because nothing has been published for it yet.

### Then declare it per project

Declaring the extension at the workspace level makes the commands exist. It does
**not** make `putnami validate` check a project. Activation is two conditions and
both are required:

1. the command's `activationFiles` match the project's tree — for `validate`,
   a `putnami.features.json`, at least one `features/*.json` or at least one
   `specs/*.json`; and
2. the project names the extension in its own `putnami.json` `extensions` array.

```json
{
  "name": "@acme/billing",
  "extensions": ["/typescript/extension", "@putnami/sdd"]
}
```

The second condition is the workspace's opt-in, and it is what lets an unused
workspace pay nothing. It is also the one **silent** way to escape the gate: a
project that starts authoring a spec and forgets the declaration is simply not
validated, and the run looks exactly like a project with nothing to validate.
This repository asserts the invariant for itself in
`workspace_adoption_test.go`; a downstream workspace owns its own.

## What you get

| Surface | What it is | Read more |
|---|---|---|
| `putnami validate` / `putnami validate-workspace` | Two DAG jobs — features and specs per project, architecture once for the workspace | [02-validation-jobs.md](02-validation-jobs.md) |
| `features`, `specs`, `architecture`, `contracts` | Four interactive command groups, eighteen subcommands | [03-commands.md](03-commands.md) |

## First run

```bash
# Author one feature, then check it.
putnami features list
putnami specs validate --projects @acme/billing

# The gate, over everything your change touched.
putnami lint,test,build,validate --impacted --enforce-coverage
```

### Keep `validate` in the agent gate

`putnami install` and `putnami context generate` write the gate into the agent
guidance of `AGENTS.md` and `CLAUDE.md`. That gate is the blocking commands of
`putnami.ci.json`, or `lint,test,build` when the workspace has no such file.
Declaring this extension does not add `validate` to it, so an agent that follows
the guidance skips spec validation. To keep `validate` in the gate, commit a
`putnami.ci.json` beside `putnami.workspace.json`:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-ci.json",
  "version": 3,
  "commands": ["lint", "test", "build", "validate"]
}
```

Then run `putnami context generate`. With Putnami Cloud, `putnami cloud ci init`
writes a `putnami.ci.json` for you; check that its `commands` list `validate`.
[`protocols/ci`](../../../protocols/ci/README.md) documents every field.

## Read an existing OpenSpec repository

You keep your OpenSpec files. The feature stays declared in
`putnami.features.json`, and its entry names the capability:

```json
{
  "protocolVersion": 3,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice-export",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers export their invoices",
    "owner": "billing",
    "target": "modeled",
    "spec": {"format": "openspec", "path": "openspec/specs/invoice-export"}
  }]
}
```

Then opt in on the project, in `putnami.json`:

```json
{ "options": { "sdd": { "specFormats": ["openspec"] } } }
```

`putnami specs validate` reads `openspec/specs/invoice-export/spec.md` as the
feature's spec. A requirement's ID is its normalized name:
`### Requirement: Export as PDF` is `export-as-pdf`. An accented letter keeps
its base letter, so `Überprüfung` is `uberprufung`. A letter with no ASCII
form, such as `ß` or a CJK character, counts as a separator, and a name with
no letter or digit left takes its position, such as `requirement-3`, which
`enforce` refuses. To keep an ID through a rename, or to give one to such a
name, write an anchor on the line above the heading:

```markdown
<!-- putnami:id export-as-pdf -->
### Requirement: Export to PDF
```

A Purpose paragraph or a requirement body over 512 bytes is still read. Its
finding names its line and a split: a warning in `report`, an error in
`enforce`.
`#### Scenario:` names are compared with the checks you declare, and a mismatch
is a warning. `openspec/changes/` is never read. The mapping is in the
[`protocols/features` README](../../../protocols/features/README.md).

The how-to
[Bring your Spec Kit, OpenSpec, Kiro or Gherkin specs](/docs/how-to/bring-your-external-specs)
takes a team from its existing specs to an enforced gate, for every format.

## If the commands are missing

`putnami features` on a workspace that has not declared the extension is not a
command. A run that plans zero jobs prints a courtesy hint that the command
moved from the core CLI to an extension, with a link to the CLI reference that
names this extension and where to declare it; the hint stays silent once the
extension is loaded.

Check what the CLI actually discovered:

```bash
putnami extensions list
```

A `@putnami/sdd` row means the workspace declares it. Its `INSTALLED` column
shows the version the lock records, not what this machine has;
`putnami extensions install` installs that version, or the latest when the
column shows `-`.

## Where the decisions are recorded

- [ADR 0013 — SDD is a standalone extension](../../cli/doc/adr/0013-sdd-as-a-standalone-extension.md)
  — why this is an extension, why architecture ships inside it, why the
  extension reads the wire and never a loader, and why one validation step is
  uncached.
- [ADR 0007 — the spec surface stays read-mostly](../../cli/doc/adr/0007-read-mostly-spec-surface.md)
- [ADR 0008 — executable architecture semantics stay in the protocol](../../cli/doc/adr/0008-executable-architecture-command-boundary.md)
