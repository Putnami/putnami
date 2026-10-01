# ADR 0001 — The template manifest declares identity, not behaviour

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/template` (`protocols/template`)

## Context

`putnami.template.json` travels inside the distribution archive, so whatever it
declares is a compatibility surface between a template packaged today and a CLI
released years later. The templating engine is local to `@putnami/cli`: it
owns the substitution syntax, variable names, discovery, archive contents, and
install location, and those change. Declaring engine behaviour in the manifest
would pin each of them in every published archive.

## Decision

The manifest declares identity and requirements only:

- identity: `name`, `description`, `version`;
- what the produced project needs: `extension`, `workspaceDevDependencies`;
- how CI exercises the template: `testVariables`;
- the `$schema` editor pointer.

1. **Rendering is not declared.** The `<%= name %>` syntax, the `.template`
   suffix, `__module__` directory substitution, and the render variables are
   engine behaviour, documented in [`../02-variables.md`](../02-variables.md),
   and change without a protocol version.
2. **Discovery, packaging, and install layout are not declared.**
3. **`testVariables` is bounded.** It overrides render variables so a template
   builds under a test project name; it cannot introduce one.
4. **Unknown fields are rejected** by both the strict decoder and the schema.
5. **No `ProtocolVersion` is declared.** The wire has no version member and the
   parser accepts one shape. This gap is the main reason the module is
   `preview`.

## Rejected alternatives

- **Declare consumed render variables.** Duplicates the `.template` files and
  adds a mismatch failure mode.
- **Declare file-processing rules.** The `.template` suffix already encodes the
  only distinction, visibly in the filename.
- **A post-create hook.** Arbitrary code execution from a downloaded archive;
  the project's extension owns post-creation work.
- **A `ProtocolVersion` constant now.** Nothing carries or branches on it.

## Consequences

- To learn how scaffolding works, read `doc/`, not the schema.
- Engine changes ship without touching published archives.
- An old archive rendered by a new CLI relies on engine backward
  compatibility, owned by the CLI's tests.
- `ValidateManifest` does not enforce the schema's `name` pattern, because
  published archives already passed it; the README states the divergence.
- A second engine would reimplement undeclared behaviour from prose, which is
  why the module is not `stable`.
