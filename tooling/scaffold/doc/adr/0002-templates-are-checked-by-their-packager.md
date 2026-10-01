# ADR 0002 — The committed templates are checked by their packager

- **Status**: accepted
- **Scope**: `@putnami/scaffold` (`tooling/scaffold`)

## Context

Nothing compiles, type-checks, lints or runs a template project. It carries
`go.mod.template` and `package.json.template`, not `go.mod` or `package.json`,
so every language toolchain ignores it by construction. It declares no test job,
because the scaffold extension contributes only `package`. Without a guard, the
first user who scaffolds from a template finds its defects: a trailing comma in
a `package.json.template`, or a web template with no `./serve` export.

## Decision

The extension that packages templates also holds them to their contract. A
conformance suite in `@putnami/scaffold` discovers every committed
`putnami.template.json` by walking the tracked tree, and asserts for each:

- the manifest validates under `go.putnami.dev/protocol/template`;
- the sibling `putnami.json` declares the same name, `type: template`, the
  scaffold extension and the `template-archives` publish channel;
- a render placeholder appears only in a `.template` file, because anywhere
  else it reaches the user as literal text;
- every `*.json.template` renders to valid JSON;
- the extension the template manifest names is the one its rendered
  `putnami.json` declares;
- a rendered application declares the `./serve` export and ships its file;
- a Go template's `go.mod` requires every `go.putnami.dev` module its sources
  import;
- the real packaging job, run over the real template in a throwaway workspace,
  produces an archive with the stamped manifest, every `.template` file and no
  `putnami.json`.

The suite is **vocabulary-free**: it checks placeholder shape and placement,
never the set of variable names. The render engine belongs to `@putnami/cli`,
and the template protocol declares it out of scope.

The template directories are declared dependencies of this project, so a change
to a template alone puts `@putnami/scaffold`'s test task in the `--impacted`
plan.

## Invariants

- Discovery walks for manifests rather than globbing a layout.
- The suite never asserts variable names.
- Language-specific checks are selected by the template's declared `extension`;
  a new language gets the generic checks until someone writes its own.
- The packaging round-trip runs the real job over the real template.

## Rejected alternatives

- **A test job per template.** Needs a language manifest in a directory that by
  construction has none.
- **Check templates from the CLI's test suite.** Puts the scaffold guard behind
  another project's gate.
- **Assert the placeholder vocabulary.** Duplicates a list the protocol refuses
  to own.
- **Scaffold and build end to end with a registry.** Needs a network and a
  registry. Rendering and building offline against the workspace framework is
  [ADR 0006](0006-templates-run-against-the-workspace-framework.md).
- **Assert a template count.** Fails every legitimate addition or removal. The
  walk asserts only that it found something.

## Consequences

- Go test caching keys on the test package's own inputs, so a run without
  `-count=1` can replay a pass after only a template changed. The gate
  (`--enforce-coverage`) sets `-count=1`; an ad-hoc run without it is not proof.
- A new language template is checked less thoroughly until its specific checks
  exist.
- The suite reads outside its own module, the price of putting the guard with
  the owner.
