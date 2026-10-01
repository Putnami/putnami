# Write a feature spec

You will write one small JSON file that says what a feature is for, what it deliberately will not do, the sentences your team agreed to, and where the durable decisions behind them are recorded. It is the artifact a reviewer — or an AI assistant — reads before touching your code, and the one thing no generator can produce for you.

Putnami calls this spec-driven development (SDD), and it keeps the spec deliberately small. Everything a tool can already see — owners, routes, schemas, migrations, current maturity — stays where it is declared. The spec adds only the intent nothing else holds.

## Before you start: declare `@putnami/sdd`

SDD is an optional extension, not part of the CLI. Without it, `putnami features`, `putnami specs`, `putnami architecture`, and `putnami contracts` do not exist, and nothing validates the files below.

Add it to `putnami.workspace.json`, then install:

```json
{
  "extensions": ["@putnami/sdd"]
}
```

```bash
putnami extensions install
putnami context generate   # teaches your AI assistant the five sdd.* MCP tools
```

Then name it in each project that authors a feature manifest or a spec, in that project's `putnami.json`:

```json
{
  "extensions": ["@putnami/typescript", "@putnami/sdd"]
}
```

Both steps are required. A project whose files match but that does not name the extension is silently skipped — the run reports nothing to validate, which looks exactly like a clean project.

`@putnami/sdd` is experimental: the manifest format, finding IDs, and detector coverage may change without a migration path.

## Declare the feature first

A spec details a feature; it never creates one. If the feature does not exist yet, declare it in `putnami.features.json` at your workspace or project root:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-features.json",
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [
    {
      "id": "billing/invoice-export",
      "type": "feature",
      "name": "Invoice export",
      "outcome": "Customers can export issued invoices",
      "owner": "billing",
      "target": "coded",
      "requirements": [
        { "id": "implementation", "stage": "coded", "evidenceKinds": ["capability"] }
      ]
    }
  ]
}
```

No feature declaration, no spec. That is the intended pressure: product intent has exactly one home.

## Write the spec

Specs are direct JSON children of `specs/` at your workspace root or at an exact project root. The filename is yours to choose — identity is the `feature` field, so renaming the file changes nothing.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-spec.json",
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": [
    "A customer exports an issued invoice without leaving the billing workspace",
    "An exported invoice stays readable after the issuing revision is superseded"
  ],
  "nonGoals": [
    "Exporting a draft invoice",
    "Exporting invoices owned by another tenant"
  ],
  "requirements": [
    {
      "id": "export-format",
      "text": "An export states its format in the response content type and never guesses it from the file name."
    }
  ],
  "decisions": [
    "billing/doc/adr/0001-invoice-export-format.md"
  ]
}
```

That is the whole contract. Six fields:

| Field | What it holds |
|---|---|
| `feature` | the one authored feature ID this spec details |
| `outcomes` | what the feature intends to deliver, in your order |
| `nonGoals` | what it deliberately excludes, in your order |
| `requirements` | stable sentences, each with a spec-local `id` |
| `decisions` | workspace-relative links to `doc/adr/*.md` records |
| `protocolVersion` | always the integer `1` |

There is no `status`, no `owner`, no `approvedAt`, no maturity, and no place to paste a route table. Those are either derived facts Putnami already knows, or process state your issue tracker already holds.

## Record the durable decisions

When a spec depends on a choice that outlives the pull request, write an architecture decision record next to the code that lives with it, at `<project>/doc/adr/NNNN-kebab-title.md`. One decision per file, never rewritten in place — a reversal gets a new record that supersedes the old one.

Keep it to four sections:

- **Context** — what forced the decision;
- **Decision** — what you do, in the present tense;
- **Rejected alternatives** — the options considered and why each lost;
- **Consequences** — what this costs and what a future author must now do differently.

Then link it from the spec by workspace-relative path. The spec never embeds the decision, and there is no JSON decision format to learn: the markdown record *is* the durable decision.

## What is checked for you

Specs are a strict, versioned wire contract, so mistakes fail loudly instead of rotting quietly:

- an unknown field — including anything that would let a spec invent a feature, a maturity stage, or evidence — is rejected, not ignored;
- `protocolVersion` must be the exact integer `1`;
- a `null` collection, a duplicate requirement ID, or a duplicate decision link fails;
- a decision link that escapes the workspace, or that is not a `doc/adr/*.md` record, fails;
- a spec that references a feature nobody declared fails, and so does a second spec for a feature that already has one;
- the file is reformatted to canonical bytes — two-space indentation, one trailing newline — so specs never produce whitespace diffs.

Your editor validates the file as you type if you keep the `$schema` line: both `https://putnami.dev/schemas/putnami-spec.json` and `https://putnami.dev/schemas/putnami-features.json` are published.

Run the checks yourself, or let CI do it:

```bash
putnami specs validate --projects @acme/billing   # just this project
putnami validate --impacted                       # the job the gate runs
```

`validate` is an ordinary job: it plans, caches, and reports like `build` and `test`, and it belongs in your verification command — `putnami lint,test,build,validate --impacted`. Contract violations fail it. Authoring gaps — a feature with no spec, a publishable project with no support entry — are reported as warnings and never change the exit code, so the reporting can grow without becoming a blocker.

A spec's requirements can also become executable: a feature declaration (manifest v2) may name acceptance checks per requirement, tests bind themselves to those checks in their runtime's native way — `spectest.Proves` in Go, `specTest` from `@putnami/runtime/spectest` in TypeScript, the `putnami_proves` pytest marker in Python — and every `test` run then records which requirements are verified. `putnami specs verify` replays the recorded verdict, and a project that sets `options.sdd.verification.specs` to `"enforce"` in `putnami.json` turns an unproven requirement into a failing gate — the default `"report"` observes without ever changing an exit code. Numeric objectives work the same way: a `threshold` criterion authors the target (a KPI over one run, or a rolling SLO with a window, environment, and freshness bound), a test publishes the measured aggregate through its runtime's measured producer — `spectest.ObserveMeasurement` in Go, `observeMeasurement` in TypeScript, the `putnami_observes` marker plus `record_property("putnami_measurement", …)` in Python — and the gate recomputes the verdict from the authored target: a producer can state a number, never its own pass. A measuring test that fails or is skipped publishes nothing, so an unproven objective can never read as green. Once a project enforces, `putnami specs baseline --update` records that floor in a committed `specs.baseline.json` in the project's directory, and `validate-workspace` fails any later change that weakens it without editing that baseline in the same change. See the [SDD command reference](/docs/spec-driven-development/commands) for the full contract, and [Spec-driven development](/docs/concepts/spec-driven-development) for how the pieces fit together.

## Why the spec stays this small

A spec that repeated derived facts would be a second inventory with no producer, stale the day after it was written. Putnami keeps one home per fact: the feature declaration owns identity and owner, build-time producers own routes and schemas and migrations, evidence owns what has actually been proven — and the spec owns only the prose none of them can emit. That is what makes it worth reading months later, and what makes it safe to hand to an assistant as context.

You now have a durable, machine-checked statement of what your feature is for, what it will not do, and which decisions it rests on — small enough that people keep it current, and strict enough that it cannot quietly disagree with your code.
