# Bring your Spec Kit, OpenSpec, Kiro or Gherkin specs

You will connect the specs your team already writes with GitHub Spec Kit, OpenSpec, Kiro or Gherkin to the Putnami spec-driven development (SDD) gate, without rewriting them. The Markdown or `.feature` file stays the one home of the prose. Putnami reads it in memory, shows what each requirement still lacks, and then holds the line once you decide to enforce.

You move in three steps: read the specs and change no exit code, make each requirement provable, then enforce. Moving a feature to the native JSON format comes last and is optional.

## Before you start: declare `@putnami/sdd`

SDD is an optional extension. Add it to `putnami.workspace.json`, then install it:

```json
{
  "extensions": ["@putnami/sdd"]
}
```

```bash
putnami extensions install
putnami context generate
```

Name it in each project that holds a feature manifest, in that project's `putnami.json`, and list the formats the project reads in `options.sdd.specFormats`:

```json
{
  "extensions": ["@putnami/typescript", "@putnami/sdd"],
  "options": {
    "sdd": { "specFormats": ["speckit"] }
  }
}
```

The formats are `speckit`, `openspec`, `kiro` and `gherkin`. A project's list replaces the one set in `putnami.workspace.json`, and the default is empty. A manifest that names a format its project did not list fails `putnami specs validate`, and the file is not read.

## Point each feature at its spec

A spec folder never creates a feature: each feature is declared in `putnami.features.json`. Its entry names its external spec with `spec`, a `format` and a workspace-relative `path`. The `spec` member needs manifest `protocolVersion` 3:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-features.json",
  "protocolVersion": 3,
  "namespace": "billing",
  "features": [
    {
      "id": "billing/invoice-export",
      "type": "feature",
      "name": "Invoice export",
      "outcome": "Customers can export issued invoices",
      "owner": "billing",
      "target": "modeled",
      "spec": { "format": "speckit", "path": "specs/001-invoice-export" }
    }
  ]
}
```

What each format reads at `path`:

| Format | `path` names | What Putnami reads |
|---|---|---|
| `speckit` | a feature directory, or its `spec.md` | `spec.md`: Success Criteria (`SC-xxx`) as outcomes, Out of Scope as non-goals, Functional Requirements (`FR-xxx`) as requirements, acceptance scenarios |
| `openspec` | a capability directory, or its `spec.md` | `## Purpose` as outcomes, each `### Requirement:` as a requirement, each `#### Scenario:` |
| `kiro` | a spec directory, or its `requirements.md` | the Introduction as outcomes, each acceptance criterion as one requirement |
| `gherkin` | a `.feature` file, or a directory of them | the `Feature:` description as outcomes, each `Rule:` as a requirement, each `Scenario:` |

Putnami never reads `plan.md`, `tasks.md` or `design.md`: they are process state or context. Only links to `doc/adr/*.md` records become decision links. Two features cannot name the same path, or a path inside another's.

## Run the gate in `report` mode

`report` is the default verification mode. Run the gate as CI does:

```bash
putnami specs validate --projects @acme/billing
putnami lint,test,build,validate --impacted
putnami specs verify
```

`specs validate` lists each finding at its file and line, such as `specs/001-invoice-export/spec.md#L42`. Fix every error it names. Warnings point at what is still loose: a scenario with no declared check, a declared check with no scenario, or a section that restates a fact Putnami already derives, such as a status or a branch. `specs verify` shows each requirement and whether a test proved it. On this first run, no requirement has a check yet, so none is proven, and `report` changes no exit code for it.

Nothing in your spec files changes. Your team can stay in `report` as long as it wants.

## Make each requirement provable

A requirement is proven when the feature declares checks for it and a test passes each check. Checks live in `putnami.features.json`, under a manifest requirement whose `id` is the requirement's ID in the spec:

```json
"requirements": [
  {
    "id": "export-format",
    "stage": "coded",
    "evidenceKinds": ["attestation"],
    "verification": { "kind": "acceptance", "checks": ["export-states-its-content-type"] }
  }
]
```

Raise the feature's `target` to `coded` or later when you add a requirement, because `modeled` allows none. A test then names the check, here in TypeScript:

```ts
import { specTest } from '@putnami/spectest';

specTest(
  'states the content type of an export',
  { feature: 'billing/invoice-export', requirement: 'export-format', check: 'export-states-its-content-type' },
  async () => {
    // exercise the export and assert its content type
  },
);
```

Go uses `spectest.Proves` and Python the `putnami_proves` marker.

### Anchor each positional ID

A Spec Kit or Kiro requirement is numbered by its position: `FR-001`, or criterion 1 of `### Requirement 1`. Insert a requirement above it and every number below shifts. Give each one an anchor, a stable ID you choose, on the line directly above it:

```markdown
### Functional Requirements

<!-- putnami:id export-format -->
- **FR-001**: An export MUST state its format in the response content type.
```

An OpenSpec requirement or a Gherkin `Rule:` needs no anchor, because its ID is its name in kebab case: `### Requirement: Export as PDF` is `export-as-pdf`. Anchor it when you rename it, or when its name holds a non-ASCII character. In a Gherkin file, the anchor is a tag among the tags above the `Rule:`: `@putnami-id:export-format`.

An anchor above a scenario names the check it stands for. In a Gherkin file, that tag is `@putnami-check:<id>` on the `Scenario:`. Without one, the scenario's check is its text in kebab case. A scenario only ever produces a warning: it never adds a check and never removes one.

## Switch to `enforce` and record the baseline

Once every requirement has its checks and a passing test, enforce in the project's `putnami.json`:

```json
{
  "options": {
    "sdd": {
      "specFormats": ["speckit"],
      "verification": { "specs": "enforce" }
    }
  }
}
```

From now on, the gate fails on an unproven requirement, on a positional requirement with no anchor, and on a requirement text over 512 bytes: split a long one into two. Record the floor in the same change:

```bash
putnami lint,test,build,validate --impacted
putnami specs baseline --update
```

`specs baseline --update` writes `specs.baseline.json` in the project directory. Commit it. A later change that drops a recorded requirement, or moves the project back to `report`, fails the gate unless it edits that file too.

## Why checks stay in the manifest, and why positional IDs need anchors

Both are decisions, recorded in [ADR 0004 of `@putnami/sdd`](/docs/spec-driven-development/adr/a-spec-may-live-in-an-external-market-format).

- **Checks stay in `putnami.features.json`.** The baseline records `feature#requirement` pairs, not scenarios. If scenarios defined the checks, deleting a scenario would remove a check, and the requirement would turn green with less proof while no gate noticed. Keeping checks in the manifest means only a reviewed manifest change can shrink the proof.
- **Positional IDs need anchors.** The gate compares requirement IDs with the baseline. A number that shifts when a requirement moves makes a moved requirement look like a removed one, and a removed one look like a moved one. An anchor gives the requirement an ID that order cannot change. `report` reads unanchored IDs, so you can start before you anchor.

## Optional: move one feature to the native format

The native `specs/*.json` format gives more guarantees than a Markdown file:

- a closed schema, where an unknown field is an error, not a silent drop;
- canonical bytes, so the file never shows a whitespace diff;
- requirement IDs written by hand, never derived from a heading or a position;
- decisions as typed links to `doc/adr/*.md` records, checked on every run.

Move one feature at a time. Preview the draft, then write it:

```bash
putnami specs init billing/invoice-export --from speckit:specs/001-invoice-export --dry-run
putnami specs init billing/invoice-export --from speckit:specs/001-invoice-export
```

The `--from` value must equal the feature's `spec` reference. The draft copies the outcomes, non-goals, requirement IDs and texts, and decision links, and adds no text of its own. It refuses what `enforce` would refuse, at the source file and line, so anchor positional IDs first. It never overwrites a file, and it has no `--all`.

`specs init` edits neither the Markdown nor the manifest. In the same change, delete the external spec and the feature's `spec` member: until you do, `specs validate` fails on two specs for one feature. A half-done move cannot merge.

You now have your existing specs behind the SDD gate: read in `report`, proven by named checks, enforced with a committed baseline, and free to move to the native format one feature at a time. See [Spec-driven development](/docs/concepts/spec-driven-development) for how the pieces fit, and the [`specs` command reference](/docs/spec-driven-development/commands) for every flag.
