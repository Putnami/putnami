# ADR 0010 — The API check compares source declarations, not type-checked packages

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`), the `validate-api` task

## Context

`validate` holds the breaking-change marker: for a stable project it compares
the exported API at the last tag of the version line with the working tree, and
fails an incompatible change that no commit declares
([doc/validate.md](../validate.md)). Issue #38 named `apidiff` or `gorelease`
as the comparison.

Both tools compare type-checked packages. To type-check the API at a tag, they
need that revision's module graph: every dependency downloaded, or present in
the module cache, at the versions the tag's `go.mod` selects. The task runs on
every `validate` and is never cached, because a new tag or commit changes its
answer without changing a file. It also runs on machines that may have no
network or no module cache for an old revision. Neither tool is a dependency of
the extension today: `apidiff` lives in `golang.org/x/exp`, and `gorelease` is
a separate command.

## Decision

**The check reads exported declarations with `go/parser` and compares their
written forms. It never compiles, type-checks or downloads a module.**

- `internal/apisurface` reads the files at the tag (`git ls-tree` and one
  `git cat-file --batch`) and in the working tree, and keeps the exported
  declarations of every importable package.
- Types are rendered with `go/types.ExprString`, which formats an expression
  without type-checking it. Before the comparison, a rendering drops parameter
  names, writes each package qualifier as its import path, replaces type
  parameters by their position, and spells `any`, `byte` and `rune` as
  `interface{}`, `uint8` and `int32`.
- `internal/apisurface/diff.go` holds the compatibility rules as a closed list:
  removals, changed signatures and types, changed kind or form, methods moved to
  a pointer receiver, and methods added to an interface another package can
  implement.

## Invariants

- The surface is a function of the file bytes alone: no network, no module
  cache, no toolchain call (`TestOnlyImportablePackagesFormTheSurface`).
- Two spellings of one type that the normalizations cover compare equal
  (`TestARenamedImportIsCompatible`, `TestPredeclaredAliasesAreCompatible`,
  `TestRenamedParametersAreCompatible`).
- Only the files the surface reads are loaded from the tag
  (`TestOnlyTheFilesTheSurfaceReadsAreLoadedFromTheTag`).

## Consequences

- The check runs offline, in well under a second on this repository, and adds
  no dependency.
- Known false positives, each listed in [doc/validate.md](../validate.md#limits):
  a method moved to an embedded type, an equivalent alias or another spelling of
  one type, an unnamed import of a package whose name differs from its path, a
  dot import, and a package that redeclares `any`, `byte` or `rune`. Today the
  only way past one is a breaking marker, which also moves the version.
- Known false negatives: changes only type-checking reveals, such as a type that
  stops being comparable, an interface that becomes sealed, or a method lost
  from a type embedded from another package.

## When to revisit

- When a reviewed waiver exists that clears a false positive without moving the
  version, measure how often it is used. Frequent use means the syntactic
  reading costs more than a type-checked one.
- When the check can rely on a module cache for both revisions, for example a
  CI step that already downloads the tag's modules, a type-checked comparison
  (`apidiff` over `go/packages`) removes both lists above.
- When TypeScript projects are checked with a type-aware tool, align the two
  checks' guarantees.

## Rejected alternatives

- **`gorelease`.** It downloads the base version from the module proxy and
  needs network access. It also reports on module versions, not on a line tag.
- **`apidiff` over `go/packages`.** It type-checks both revisions, so the tag's
  revision has to be checked out and its dependencies resolved on every run.
  It also adds `golang.org/x/exp` and `golang.org/x/tools` to the extension.
