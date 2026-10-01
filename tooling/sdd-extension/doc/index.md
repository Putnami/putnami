# Spec-driven development

`@putnami/sdd` is the extension that turns recorded intent into a CI verdict.
It owns three kinds of durable declarations and the jobs that hold code to
them:

- **Features and specs** — `putnami.features.json` declares what a project is
  for, requirement by requirement; one spec per feature records outcomes,
  non-goals, and the decisions it rests on. Tests bind themselves to
  requirements, and a project that opts into `enforce` fails its gate when a
  requirement is unproven.
- **ARC — Architecture Rules as Code** — each domain declares one
  `putnami.architecture.json`: the projects it owns, what it exports, and what
  it imports from other domains. The validator compares the declared
  permissions against the workspace's actual project dependencies; an
  undeclared cross-domain dependency fails.
- **DARC — Domain Access & Replication Contracts** — the import half of an
  architecture manifest. A consumer states *how* it accesses another domain's
  data: a stable reference, a query, an immutable snapshot, a rebuildable
  projection, or a command. The central rule: **data may be copied; authority
  may not be copied.**

The extension is **experimental** and opt-in: a project is validated only when
it declares `/tooling/sdd-extension` in its `putnami.json` `extensions` array.
The wire formats may change without a migration path.

## Authoring an architecture manifest

Three surfaces, in the order you meet them:

| | What it does | Language |
|---|---|---|
| `putnami architecture init` / `sync` | Scaffolds a domain and reconciles the mechanical half of existing manifests — see [Commands](03-commands.md#init-and-sync--the-authoring-half) | any |
| [`go.putnami.dev/app/darc`](https://putnami.dev/docs/frameworks/go/domain-access-contracts) | Enforces a declared import at run time, and emits the evidence `architecture validate` reads | Go |
| [`@putnami/application` → `darc`](https://putnami.dev/docs/frameworks/typescript/domain-access-contracts) | The same, for a TypeScript workload | TypeScript |
| [`go.putnami.dev/sdk/extension/architecture`](../../extension-sdk/doc/08-architecture-manifest.md) | Authors a manifest in Go and pins the committed file to that program | Go |

Both framework runtimes enforce the same contracts, and neither is its own
oracle: they answer one shared corpus of contracts and ordered operations in
[`protocols/architecture/fixtures/conformance/`](../../../protocols/architecture/fixtures/conformance/README.md).
A behavior that differs between them fails on one side rather than shipping as
two runtimes that describe the same manifest differently. Fix a divergence by
deciding which runtime is right — never by editing the corpus.

The builder is the one Go-only surface, because extensions are Go: it authors a
manifest from a Go program and pins the committed JSON to it. It is optional in
Go too. Every domain, in any language, authors with `init`/`sync` and is held to
the same gate.

## Reading order

1. [Getting started](01-getting-started.md) — activate the extension and author
   your first feature and spec.
2. [The validation jobs](02-validation-jobs.md) — what `validate` and
   `validate-workspace` check, what fails, what warns, and how the results
   cache.
3. [Commands](03-commands.md) — the full `features`, `specs`, `architecture`,
   and `contracts` reference.
4. [MCP tools](04-mcp-tools.md) — the read-only `sdd.*` tools an agent uses to
   ask what a feature is for or inspect one exact architecture domain before
   touching its code.
5. [Parity](05-parity.md) — how the extension proves it matches the behavior it
   replaced.

For the narrative — why these declarations exist and how they constrain both
people and agents — read
[Spec-driven development](https://putnami.dev/docs/concepts/spec-driven-development)
in the concepts section.
