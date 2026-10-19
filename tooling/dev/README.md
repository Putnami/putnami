# `@putnami/dev`

The development verb vocabulary. The extension's manifest declares the eleven
development verbs, in the order help lists them, with the description,
aliases, related commands and default traits of each. It runs none of them:
the language extensions declare and run the jobs of every verb.

The Go, TypeScript and Python extensions name `@putnami/dev` in their
`extensionDependencies`, so installing any of them installs this extension
too ([ADR 0066](../cli/doc/adr/0066-an-extension-installs-and-loads-its-extension-dependencies.md)).
How a loaded vocabulary drives help, aliases, completion and default traits is
[ADR 0067](../cli/doc/adr/0067-putnami-dev-declares-the-development-verbs.md);
what this extension declares is [ADR 0001](doc/adr/0001-the-dev-extension-declares-the-development-verbs.md).

## The verbs

| Verb | Aliases | Default traits |
|---|---|---|
| `build` | `b` | none |
| `test` | `t` | none |
| `lint` | `l` | none |
| `serve` | `s` | requires a runnable project |
| `run` | none | requires a runnable project |
| `format` | `f` | none |
| `package` | none | injects the project's publish channels |
| `publish` | `p`, `P` | injects the publish channels; changes a registry |
| `deploy` | `d`, `D` | changes a cloud |
| `generate` | none | none |
| `validate` | none | none |

Each description, alias list and related-command list equals the one the core
shows for the verb, and each verb's traits equal the core's defaults. Loading
the vocabulary therefore changes no help text and no job's traits.

## Use it

A workspace receives the extension through a language extension. To declare
it directly:

```json
{
  "extensions": ["@putnami/dev"]
}
```

This repository declares it by path (`/tooling/dev`), which satisfies the
dependency of every language extension the repository loads by path.

## Runtime

`cmd/putnami-dev` answers the runtime handshake
(`putnami-dev __putnami runtime-info`) and refuses every other invocation.
`@putnami/go` builds it and packages it into the extension archive. The
manifest declares no `runtime`, so no host starts it.

## Support status

- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json).
- **What may still change**: the verbs this extension declares, their
  presentation, and the commands it adds.
- **Evidence**: the [development verbs specification](specs/development-verbs.json)
  and the tests under `cmd/putnami-dev`.
