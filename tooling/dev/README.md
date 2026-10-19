# `@putnami/dev`

The development verb vocabulary and the version commands. The extension's
manifest declares the eleven development verbs, in the order help lists them,
with the description, aliases, related commands and default traits of each.
It runs none of them: the language extensions declare and run the jobs of
every verb. Its executable serves `putnami version get` and
`putnami version tag`.

The Go, TypeScript and Python extensions name `@putnami/dev` in their
`extensionDependencies`, so installing any of them installs this extension
too ([ADR 0066](../cli/doc/adr/0066-an-extension-installs-and-loads-its-extension-dependencies.md)).
How a loaded vocabulary drives help, aliases, completion and default traits is
[ADR 0067](../cli/doc/adr/0067-putnami-dev-declares-the-development-verbs.md);
what this extension declares is [ADR 0001](doc/adr/0001-the-dev-extension-declares-the-development-verbs.md).
How it serves the version commands is
[ADR 0002](doc/adr/0002-the-dev-extension-serves-version-get-and-tag.md).

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

## The version commands

| Command | What it does |
|---|---|
| `putnami version get [--scope <line>]` | Prints each version line with the version HEAD has on it, or only the line `--scope` names. `--output=json` and `--output=jsonl` print one result envelope. |
| `putnami version tag [<tag>] [--scope <line>] [--push] [--dry-run] [--yes]` | Releases one line: writes the line's `CHANGELOG.md`, commits it, and puts an annotated tag on that commit. `--push` pushes HEAD and the tag. Without `--yes`, it only proposes the release. |

`putnami version` runs `get`. A workspace with more than one line names the
line to release with `--scope`. A `<tag>` replaces the proposed release when
it matches the line's tag pattern.

Both are interactive subcommands. putnami launches the executable in the
workspace root and opens a host RPC channel, through which the command asks
the core for the version lines (`version.lines`). The core computes every
version; the executable prints it, and `version tag` runs git to commit, tag
and push. A hosted run (`--credential-fd`) opens no channel, so neither
command runs there.

A usage error exits 2. `version tag` refuses with exit status 2 when the
workspace has several lines and no `--scope`, when HEAD is already tagged,
when the tree is not clean, when the tag exists, when the tag does not match
the line's pattern, when `PUTNAMI_SOURCE_REVISION` binds the run to a commit
other than HEAD, and under `--output=json` or `--output=jsonl`. A failure of
git or of the host exits 1, and so does a dependency graph the host could not
refresh.

## Runtime

`cmd/putnami-dev` answers the runtime handshake
(`putnami-dev __putnami runtime-info`), serves the version commands, and
refuses every other invocation with exit status 2. The manifest declares it as
the extension's `runtime`. In a workspace that loads the extension by path,
`bin/prepare` builds it with the Go release `putnami.lock.json` pins.
`@putnami/go` builds it and packages it into the extension archive that a
workspace installs by name.

## Support status

- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json).
- **What may still change**: the verbs this extension declares, their
  presentation, and the commands it adds.
- **Evidence**: the [development verbs specification](specs/development-verbs.json),
  the [version commands specification](specs/version-commands.json), and the
  tests under `cmd/putnami-dev` and `internal`.
