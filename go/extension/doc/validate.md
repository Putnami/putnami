# Validate

**Command:** `putnami validate [project]`

For every Go project the support catalog lists as stable, `validate` compares
the exported API with the last tag of the project's version line. An incompatible change fails the command
unless a commit since that tag declares it breaking. A project that ships a CLI
can hold its commands and flags to the same rule with a committed
[command surface](#command-surface). The breaking-change marker
is what makes `putnami version` advance the line past a feature release, so a
stable package cannot break its users in a release that promises it does not.

**Activation:** Any project containing `go.mod` or `*.go` files. Other
extensions contribute their own steps to the same `validate` command.

## Which projects are checked

The check reads the root [`putnami.support.json`](../../../protocols/support/README.md):

| Catalog says | Check |
|---|---|
| `stable` | runs |
| `preview` or `experimental` | skipped: these promise no compatibility |
| nothing about the project | skipped: an unlisted project promises nothing |
| no catalog in the workspace | runs for every project |
| an invalid catalog | fails the task |

When a catalog exists, it is the authority: only what it lists as stable is
checked, and a skipped project says why in an information line. A workspace
without a catalog has made no statement, so every project is checked.

The version bump also counts an unlisted project that a stable project depends
on as stable, because its code ships inside the stable product. This check does
not: it guards the API a project publishes, and an unlisted project publishes
none. A breaking change there still needs its marker for the bump.

A project is looked up as a `protocol` subject when it carries the `protocol`
tag, and as a `package` subject otherwise, under its project name. The tags
are the project's own `tags` in `putnami.json`, else the tags of its scopes; an
authored `"tags": []` blocks the scopes' tags. The version bump classifies
projects the same way.

## What it compares

1. The baseline is the last tag of the project's version line that HEAD can
   reach: the line of the nearest ancestor scope that declares one, else the root
   line `v{version}`. A line with no reachable tag has no released API, so the
   check passes and says so.
2. The API is read from source with `go/parser`, from the files at that tag and
   from the working tree, uncommitted edits included. Nothing is compiled or
   type-checked, and no module is downloaded.
3. Every importable package of the module counts. A directory is left out when a
   path segment is `internal`, `testdata` or `vendor`, starts with `.` or `_`, or
   sits below a nested `go.mod`. `main` packages, `_test.go` files, and files
   whose `//go:build` constraint holds on no platform are left out too.

These changes are incompatible:

| Change | Example message |
|---|---|
| A package removed | `go.example.com/lib/client: package removed` |
| An exported function, type, variable or constant removed | `func Greet removed` |
| A function or method signature changed | `func Greet changed from func(string) string to func(string, bool) string` |
| A function or type changed its type parameters | `type Set changed its type parameters from [K comparable] to [K any]` |
| A method of an exported type removed, or moved from a value to a pointer receiver | `method Client.Do removed` |
| An exported struct field removed, or its type changed | `field Config.Port changed type from int to string` |
| A method added to or removed from an interface, or an embedded interface changed | `interface method Store.Put added: every implementation outside the package must add it` |
| The explicit type of a variable or constant changed | `const Limit changed type from int to int64` |
| A declaration changed kind or form | `func Handler is now a var`, `type Opts changed from a struct to an interface` |

Additions are compatible: a new package, function, type, method, field or
constant breaks no caller. These changes are compatible too:

- A method added to a sealed interface: one with an unexported method, or one
  that embeds a sealed interface of the same package. No other package can
  implement it.
- A renamed parameter, result or type parameter: signatures are compared by
  their types and by type parameter position, never by name.
- A renamed or added import name: a package qualifier is compared by the import
  path the file gives it, so `foo.T` and `bar.T` are equal when `foo` and `bar`
  import the same path. Messages write qualified types with the import path,
  such as `go.example.com/lib/model.User`.
- A predeclared alias for its type: `any` and `interface{}`, `byte` and
  `uint8`, `rune` and `int32` compare equal.

## Command surface

A project that ships a command-line tool can hold its commands and flags to the
same rule. It commits a
[command-surface document](../../../protocols/cli/doc/06-command-surface.md)
that lists the commands users type, with their flags and positionals, and the
global flags. The `command-surface` option names it, relative to the project,
in the project's `putnami.json`:

```json
{
  "options": {
    "@putnami/go:validate": {
      "command-surface": "command-surface.json"
    }
  }
}
```

The check reads the document from the working tree, at the path the option
names now, and from the tag it compares the API with, at the path the
project's `putnami.json` named at that tag. So a document that moves in the
same change is still compared with the released one. The check reads the
declaration at the tag even when the project declares no document now: a
project that removes the option after a release gets a warning until its next
tag, so removing it does not end the comparison without a trace. At the tag,
the check reads the option from the project's `options` under `validate`,
`@putnami/go` and `@putnami/go:validate`, the last one winning, as the CLI
merges them. Workspace options and command-line flags are not read at the
tag: declare the option in the project's `putnami.json`. Both sides follow the
CLI's rule for the option's two spellings: a layer can write
`command-surface` or `commandSurface`, and when one layer writes both,
`commandSurface` wins.

The document in the working tree must be a `.json` file outside the
directories the task's [cache key](#caching) does not read: a directory whose
name starts with `.`, `node_modules`, `out`, `dist` or `vendor`. Any other
path fails the task, because a change to it would leave a stored verdict in
place. The document at the tag has no such rule.

These changes are incompatible:

- a command removed;
- a flag removed, global or of a command, or a short alias removed;
- a flag that starts or stops taking a value;
- a value removed from a flag's closed list, or a closed list given to a flag
  that accepted any value;
- a positional removed, an optional positional that becomes required, or a new
  required positional.

Additions are compatible: a command, a flag, a short alias, an accepted value
or an optional positional. The document's
[compatibility rules](../../../protocols/cli/doc/06-command-surface.md#compatibility)
list them with their messages.

Each incompatible change is one error diagnostic with the code `api-compat`. It
names the command and the flag, and points at the document:

```text
command "ci explain": flag --event removed since tooling/v0.3.0; declare the breaking change with "!" or a BREAKING CHANGE: footer
```

The same breaking-change marker allows the change, and the same rules decide
whether the project is checked and whether a tag exists to compare with. The
document adds these cases:

| Case | Result |
|---|---|
| The project declared no document at the tag | an information line; the API is still compared |
| The project declared a document at the tag and declares none now | a warning coded `api-compat-not-compared` on the project's `putnami.json`; the API is still compared |
| The project declared a document at the tag, but the tag does not hold it | a warning coded `api-compat-not-compared`; the API is still compared |
| The document at the tag has a later protocol version than this extension reads | a warning coded `api-compat-not-compared`; the API is still compared |
| The project's `putnami.json` at the tag does not parse, or sets the option to something other than a path of the project, whether or not the project declares a document now | a warning coded `api-compat-not-compared`; the API is still compared |
| The declared document is missing from the project, is a directory, is reached through a symbolic link, or is named in another case than on disk | the task fails |
| The document does not parse, in the working tree or at the tag, in a protocol version this extension reads | the task fails |
| The option is an absolute path or a path out of the project | the task fails, even for a project the check skips |
| The option names a file that is not `.json`, or a file inside a directory the cache key does not read | the task fails, even for a project the check skips |

A warning stands for what no marker could fix: the check cannot read the
released document, or the project no longer declares the current one, so it
compares nothing rather than fail every run until the next tag. A newer
`@putnami/go` reads every earlier protocol version of the document.

The CLI produces the document. Putnami's own CLI renders it from its command
catalog and commits it as `tooling/cli/command-surface.json`, and a test fails
when the committed bytes differ from the catalog, so a change to a command
always shows in the reviewed diff.

## The breaking-change marker

The check reads the commits from the tag to HEAD that touch the project
directory. When one of them is a conventional commit that declares a breaking
change, every incompatible change passes and is listed as information, and the
task's data holds `breakingDeclared: true`. The output names no commit:

```text
go.example.com/lib: func Greet removed since lib/v0.4.0; a commit since lib/v0.4.0 declares the breaking change
```

Either form of the marker counts:

```text
feat!: drop the greeting
```

```text
refactor: drop the greeting

BREAKING CHANGE: Greet is gone; use Wave.
```

A message counts exactly when the version bump counts it: a subject of the form
`type(scope)!: summary`, or `type: summary` with a body line that starts with
`BREAKING CHANGE:` or its synonym `BREAKING-CHANGE:`. A marker on a commit that
does not touch the project does not count. An uncommitted change has no marker
yet: commit it with one.

### Squash merges

A squash merge replaces the branch's commits with one commit whose message is
the pull request title and body. The marker on a branch commit lets the check
pass on the pull request, but it does not reach the main branch. Put the marker
in the pull request title as well, for example `feat(go)!: drop Greet`, so the
commit on main declares the break and the version bump sees it. This change
does not enforce the title: `validate` on the main branch reports a dropped
marker after the merge, as an unmarked incompatible change.

## Failure output

Each incompatible change is one error diagnostic with the code `api-compat`,
naming the import path and the symbol:

```text
go.example.com/lib: func Greet removed since lib/v0.4.0; declare the breaking change with "!" or a BREAKING CHANGE: footer
```

A changed declaration also carries its file and line. A change to a command
surface names the command and the flag instead, and points at the document.
Fix the code, or keep the change and commit it with the marker.

The summary counts the two kinds apart, for example `1 incompatible API change
and 2 incompatible command-surface changes since tooling/v0.3.0`. A failure
reports the `api-incompatible` metric with the API changes, and, for a project
that declares a command surface, the `command-surface-incompatible` metric with
the command-surface changes. The task's data holds the sum as `incompatible`,
and the command-surface part as `commandSurfaceIncompatible`.

For a project that declares a command surface, and for one that declares none
now but gets a command-surface warning, the task's data also holds
`commandSurfaceCompared`, whether the document was compared with the tag, and
`commandSurface`, the document's workspace-relative path when the check knows
it: the path the project declares, or else the one it declared at the tag.
When the document was not compared, `commandSurfaceNotCompared` holds the note
or the warning that says why.

## Caching

The task is cached. Its key holds what the verdict reads:

- the project's non-test Go files, which are the files the API comes from,
  and the files their `//go:embed` directives select;
- the project's `go.mod` files and `.json` files;
- the root `putnami.support.json`;
- the `command-surface` option, in both spellings;
- the project's release baseline, which the CLI reads from git before the task
  runs (the `releaseBaseline` runtime input): the repository state, the line's
  tag pattern, the last tag of the line HEAD reaches, the tree that tag holds
  at the project directory, and whether a commit since that tag that touches
  the project declares a breaking change.

The project's file patterns skip directories whose name starts with `.`, where
a run writes its generated files, and the CLI's file walk never enters
`node_modules`, `out`, `dist` or `vendor`. The check reads no Go file under any
of those directories, in the working tree or at the tag, so a package there is
never compared.

The key names no commit. A pull request and the commit its squash merge puts
on the main branch share the verdict when they hold one tree and their
messages declare the same break: put the marker in the pull request title. A
squash title that adds or drops the breaking marker (`!` or a
`BREAKING CHANGE:` footer) moves the key, and the verdict moves with it. A new
tag, a moved tag, a commit that adds or drops a marker, or a shallow clone
changes the key. A skip for a project the catalog does not list as stable is
cached as well. The input needs a CLI that implements CLI contract 7; an older
CLI refuses this extension. See CLI
[ADR 0062](../../../tooling/cli/doc/adr/0062-a-release-baseline-input-names-the-baseline-not-the-commit.md).

## Limits

- **Checkouts that cannot compare.** The task passes without comparing, with a
  warning diagnostic coded `api-compat-not-compared`, when:
  - the workspace is not in a git repository, for example a copied snapshot;
  - the clone is shallow, so it lacks the tags and history the check needs;
  - no tag of the line is reachable from HEAD while the repository has tags of
    other lines, which suggests the line's tags were not fetched.

  A clone with no tags at all, such as one fetched with `--no-tags`, reads as a
  line with no release: it passes with an information line, not a warning.
  Fetch the full history and the tags, for example with `fetch-depth: 0` on
  GitHub Actions, to run the check in CI.
- **No type information.** Types are compared by how they are written, after
  the normalizations above. These compatible changes still read as
  incompatible:
  - moving a method to an embedded type, which keeps it in the method set;
  - replacing a type with an equivalent alias or another spelling of the same
    type;
  - importing a package without a name in one version and with one in the
    other, when the package's name differs from the name its import path
    implies (the last path element, without a major version suffix or a `go-`
    prefix);
  - a dot import, whose names are compared unqualified;
  - a package that declares its own `any`, `byte` or `rune`.

  No waiver exists yet: a false positive can only be cleared with a breaking
  marker, which also moves the version. A change that only type-checking
  reveals, such as a type that stops being comparable, or an interface that
  becomes sealed, is not detected.
- **Tags a language provider reports.** The workspace loader also reads tags a
  language provider reports for a project that declares none. The task cannot
  see those, so such a project takes its scopes' tags when it chooses between a
  `protocol` and a `package` subject.
- **Platform files.** Files for every platform are read together. When two
  files declare the same name, the first in path order is compared.
- **Go only.** TypeScript projects are not checked yet, and the command
  surface is compared only for a Go project.
- **What the command surface holds.** The check holds what the document lists,
  and Putnami's own CLI lists its core command catalog only:
  - commands and flags an extension manifest declares, such as the
    `--enforce-coverage` flag of `test` or the commands of the `specs` group,
    are not in the document, so removing one is not caught;
  - global flags carry no value list, because the catalog lists their values
    as completion candidates, so a value removed from a global flag such as
    `--output` is not caught.

The choice of a source-level comparison over `apidiff` or `gorelease` is
recorded in [ADR 0010](adr/0010-the-api-check-compares-source-not-types.md).
