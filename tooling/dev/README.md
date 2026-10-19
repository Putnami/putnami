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

| Verb | Aliases | Default traits | Kind, workload and effects |
|---|---|---|---|
| `build` | `b` | none | `files` kind |
| `test` | `t` | none | `verification` kind |
| `lint` | `l` | none | `diagnostics` kind |
| `serve` | `s` | requires a runnable project | `serve` workload |
| `run` | none | requires a runnable project | `once` workload |
| `format` | `f` | none | rewrites sources |
| `package` | none | injects the project's publish channels | none |
| `publish` | `p`, `P` | injects the publish channels; changes a registry | none |
| `deploy` | `d`, `D` | changes a cloud | consumes the release set |
| `generate` | none | none | none |
| `validate` | none | none | `criteria` kind |

The core CLI declares none of these verbs. Help and completion take each
verb's description, aliases and related commands from this vocabulary, and the
CLI runs a verb where a loaded extension declares its command. The kind, workload and
effects need CLI contract 11, and the manifest claims it. A job takes them from
its verb unless its command declares its own `traits`, which replace the
verb's wholesale. The CLI reads a command's workload, source rewriting and
side effects from these traits, not from its name; the [verbs
reference](../../protocols/extension/doc/06-verbs.md) defines each value.

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
