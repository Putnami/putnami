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

Then install and regenerate the agent context, so assistants learn the five
`sdd.*` MCP tools exist:

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
   a `putnami.features.json` or at least one `specs/*.json`; and
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
| `sdd.list_features`, `sdd.feature_context`, `sdd.list_specs`, `sdd.spec_context`, `sdd.architecture_context` | Five read-only MCP tools | [04-mcp-tools.md](04-mcp-tools.md) |

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

A `Skipped` row names the reason. The most common one is a runtime that has not
been prepared yet, which `putnami extensions install` fixes.

## Where the decisions are recorded

- [ADR 0013 — SDD is a standalone extension](../../cli/doc/adr/0013-sdd-as-a-standalone-extension.md)
  — why this is an extension, why architecture ships inside it, why the
  extension reads the wire and never a loader, and why one validation step is
  uncached.
- [ADR 0007 — the spec surface stays read-mostly](../../cli/doc/adr/0007-read-mostly-spec-surface.md)
- [ADR 0008 — executable architecture semantics stay in the protocol](../../cli/doc/adr/0008-executable-architecture-command-boundary.md)
