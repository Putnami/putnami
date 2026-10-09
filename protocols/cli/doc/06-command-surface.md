# Command surface v1

A CLI's command surface is what its users type: the commands, their flags and
positionals, and the flags every command accepts. A script that worked against
one release keeps working against the next only when that surface did not
lose anything it used. The command-surface document states the surface of one
CLI, so a release can be compared with the last one.

The CLI commits the document and derives it from its own command catalog, so
it changes only when the catalog does. Descriptions, usage lines, value names
and examples are not part of it: rewording help never changes the document.

```json
{
  "protocolVersion": 1,
  "globalFlags": [
    { "long": "--output", "type": "value" },
    { "long": "--verbose", "short": "-v", "type": "bool" }
  ],
  "commands": [
    { "path": "build", "flags": [], "positionals": [] },
    {
      "path": "ci explain",
      "flags": [
        { "long": "--event", "type": "value", "values": ["pull_request", "push", "tag"] }
      ],
      "positionals": []
    },
    {
      "path": "projects tag",
      "flags": [{ "long": "--remove", "type": "bool" }],
      "positionals": [
        { "name": "name", "required": true },
        { "name": "tag", "required": true }
      ]
    }
  ]
}
```

| Member | Meaning |
| --- | --- |
| `protocolVersion` | Always `1`. |
| `globalFlags` | The flags the CLI accepts with any command, sorted by `long`. |
| `commands` | The commands users type, sorted by `path`. An alternate spelling of another command is a command of its own. |
| `path` | The space-separated invocation path after the program name, such as `projects list`. |
| `flags` | A command's own flags, sorted by `long`. |
| `positionals` | A command's positional arguments, in invocation order. |
| `long` | The long spelling with its dashes, such as `--dry-run`. |
| `short` | The single-dash alias, such as `-g`. Absent when the flag has none. |
| `type` | `bool` for a flag that takes no value, `value` for one that does. |
| `values` | The closed list of values a `value` flag accepts, sorted. Absent when the flag accepts any value. |
| `name` | A positional's name as the usage line spells it. |
| `required` | Whether an invocation must supply the positional. |

Every member is present except `short` and `values`, and none is null. Lists
are sorted by byte order, and a path, a long spelling, a short alias within one
flag list and a value within one list each appear once. Readers refuse unknown
members, trailing JSON and documents over 1 MiB. The canonical bytes use
two-space indentation and end with a newline.

## Compatibility

Comparing the document of a release with the current one gives the changes
that can break an invocation that used to work:

| Incompatible | Example message |
| --- | --- |
| A command removed | `command "ci init" removed` |
| A flag removed, global or of a command | `command "ci init": flag --force removed` |
| A short alias removed or changed | `global flag --verbose lost its short alias -v` |
| A flag that starts or stops taking a value | `command "ci init": flag --force now takes a value` |
| A value removed from a closed list | `global flag --output no longer accepts "text"` |
| A closed list given to a flag that accepted any value | `command "ci init": flag --tag now accepts only a, b` |
| A positional removed | `command "projects tag": positional "tag" (position 2) removed` |
| An optional positional that becomes required | `command "projects tag": positional "tag" (position 2) is now required` |
| A new required positional | `command "build": new required positional "target" (position 1)` |

Everything added is compatible: a command, a flag, a short alias, a value, an
optional positional. So are a closed list that is removed, a positional that
becomes optional, and a positional renamed in place: positionals are compared
by position, not by name.

## API

Go publishes the document from `go.putnami.dev/protocol/cli`:

| Symbol | Purpose |
| --- | --- |
| `CommandSurface`, `CommandSurfaceCommand`, `CommandSurfaceFlag`, `CommandSurfacePositional` | The document |
| `NewCommandSurface` | Builds the canonical document from unsorted input, and refuses a duplicate |
| `MarshalCommandSurface` | The canonical bytes of a valid document |
| `ParseCommandSurface` | Strictly decodes and validates one document |
| `IncompatibleCommandChanges` | The incompatible changes from one document to another, as `CommandChange` values sorted by command, flag and message |

The schema is [`command-surface.json`](../schemas/command-surface.json). Go is
the document's only runtime: `@putnami/cli-protocol` has no reader for it. A
test pins the schema's members, required members, patterns and vocabulary to
the Go types, and a [corpus](../testdata/command-surface/documents.json) pins
which documents a reader accepts.

## Producers and consumers

`@putnami/cli` commits its surface as `tooling/cli/command-surface.json`,
rendered from its command catalog and pinned by a test that fails when the
committed bytes differ.

The `validate` task of `@putnami/go` reads the document a stable project names
with its `command-surface` option, compares it with the one at the last tag of
the project's version line, and fails an incompatible change that no commit
since the tag declares as breaking. See
[`go/extension/doc/validate.md`](../../../go/extension/doc/validate.md).
