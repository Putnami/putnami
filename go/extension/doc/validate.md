# Validate

**Command:** `putnami validate [project]`

For every stable Go project, `validate` compares the exported API with the last
tag of the project's version line. An incompatible change fails the command
unless a commit since that tag declares it breaking. The breaking-change marker
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
| nothing about the project | runs: an unreviewed project counts as stable |
| no catalog in the workspace | runs for every project |
| an invalid catalog | fails the task |

A project is looked up as a `protocol` subject when it carries the `protocol`
tag, and as a `package` subject otherwise, under its project name. The version
bump classifies projects the same way.

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
constant breaks no caller. Two other changes are compatible too:

- A method added to an interface that has an unexported method: no other
  package can implement such an interface.
- A renamed parameter, result or type parameter: signatures are compared by
  their types and by type parameter position, never by name.

## The breaking-change marker

The check reads the commits from the tag to HEAD that touch the project
directory. When one of them is a conventional commit that declares a breaking
change, every incompatible change passes and is listed as information:

```text
feat!: drop the greeting
```

```text
refactor: drop the greeting

BREAKING CHANGE: Greet is gone; use Wave.
```

A message counts exactly when the version bump counts it: a subject of the form
`type(scope)!: summary` or `type: summary` with a body line that starts with
`BREAKING CHANGE:`. A marker on a commit that does not touch the project does
not count. An uncommitted change has no marker yet: commit it with one.

## Failure output

Each incompatible change is one error diagnostic with the code `api-compat`,
naming the import path and the symbol:

```text
go.example.com/lib: func Greet removed since lib/v0.4.0; declare the breaking change with "!" or a BREAKING CHANGE: footer
```

A changed declaration also carries its file and line. Fix the code, or keep the
change and commit it with the marker.

## Caching

The task is never cached. Its verdict reads the line's tags, the files at the
last one, and the messages of the commits since, and a new tag or a new commit
changes the answer without changing any file a cache key could name.

## Limits

- **Shallow clones.** A shallow clone lacks the tags and history the check
  needs. The task warns and passes without comparing. Fetch the full history,
  for example with `fetch-depth: 0` on GitHub Actions, to run the check in CI.
- **No type information.** Types are compared by how they are written. Moving a
  method to an embedded type keeps it in the method set but reads as removed;
  replacing a type with an equivalent alias reads as a change. A change that
  only type-checking reveals, such as a type that stops being comparable, is not
  detected.
- **Platform files.** Files for every platform are read together. When two
  files declare the same name, the first in path order is compared.
- **Go only.** TypeScript projects are not checked yet.
