# ADR 0004 — The API check parses source and does not type-check

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`), the `validate-api` task

## Context

`validate` holds the breaking-change marker: for a stable package it compares
the public API at the last release of the version line with the working tree, and
fails a change that breaks importers and that no commit declares
([doc/11-validate.md](../11-validate.md)). The request named `api-extractor` as
the comparison.

`api-extractor` reads the declaration files of a package and writes an API
report. It does not read source, so both sides must first be compiled to
declarations with `tsc`. For the tag, that means checking the revision out and
installing the dependencies its lockfile selects, on every run whose key
moves: a new commit, a new release or an edit of the package. It also runs on
machines that may have no network.
`api-extractor` is not a dependency of the extension today, and its report is
a text for a reviewer: it does not tell an incompatible change from a
compatible one.

The Go extension made the same choice for the same reasons: it compares
declarations as they are written and does not type-check
(`go/extension/doc/adr/0010-the-api-check-compares-source-not-types.md`).

TypeScript adds one constraint that Go does not have. Go ships a parser in its
standard library. TypeScript syntax needs a real parser: generics, arrow
functions, template literals, regular expressions and JSX cannot be told apart
by a scanner without parsing them. A hand-written one would turn each of its
mistakes into a false failure that only a breaking marker clears.

The marker decides the version bump. The check only catches a forgotten
marker, and its two mistakes do not cost the same. A wrong failure can only be
cleared with a marker, which forces a release that announces a break nobody
suffers. A missed break leaves the package where it was before the check
existed. A first version failed on every change of written type. On this
repository it failed routine compatible edits, such as a callback parameter
whose return type gained `undefined`, and it still missed breaks that need
type information.

## Decision

**The check parses both sides with the TypeScript parser the workspace already
installs, in one bun process, and compares the declarations as written. It
never type-checks, compiles or installs.**

- `internal/apisurface/extract.mjs` receives the file texts of both sides on
  stdin. It calls `ts.createSourceFile` on the files the entry points reach and
  returns each exported declaration. It reads no source file from disk.
- The `typescript` package is the one that resolves from the project: the
  manifest's `workspaceDevDependencies` installs it for the `types` build
  phase. The extension finds its directory itself and starts bun with
  `--no-install`, so nothing is fetched.
- The files at the tag come from `git ls-tree` and one `git cat-file --batch`.
  The working tree's files are the ones git tracks or does not ignore, so both
  sides follow one rule.
- Before the comparison, a rendering drops parameter names, replaces type
  parameters by their position, writes `Array<T>` as `T[]`, writes string
  literals in one quote style, and orders the members of a union and of an
  object type.
- **The check fails only on a change that breaks an importer whatever the
  types are.** That is a closed list: an entry point, an export or a member
  removed; a value that becomes a type only or the reverse; a class or an enum
  that becomes another kind of declaration; a class that becomes abstract; an
  enum that becomes a `const enum`; a member or a constructor with less
  visibility; a property that can no longer be written; a constructor that
  took parameters removed.
- **Every change of written type is a warning and never fails.** Signatures,
  the types of variables and members, type aliases, type parameters, heritage
  clauses and members added as required are compared and reported. Whether
  one breaks an importer depends on assignability, which the check does not
  compute. The author decides and adds the marker. No change of written type
  is recognized as compatible: a rule that hid a widened parameter only
  removed warnings, could never turn a failure into a pass, and was deleted.
- **A removal the check cannot confirm is a warning.** A member that a base
  the check cannot read may declare, and an export that an `export *` the
  check cannot follow may still export, are reported without failing. So is
  another kind of declaration that keeps its uses: an interface written as a
  type alias, a function written as a variable.
- **An entry point is a subpath.** It exports what any of its conditions
  exports, so restructuring the conditions of a subpath changes nothing.
- The reader follows a name that the written types of an exported declaration
  use, when the package declares it without exporting it. It stays in the
  files of the package, reads each declaration once, and keeps two
  declarations of one name in two files apart. Every change in such a
  declaration is a warning, a removed member included: importers have no name
  for the type.
- A heritage clause is resolved in the file that writes it, on both sides. A
  member that moves to a base the check reads is not a removal, and a base of
  the same name in another file is never taken for it. A member inherited at
  the tag counts as declared: dropping a base removes its members.
- `internal/apisurface/diff.go` holds the rules, in Go, where they are tested
  without bun.
- The catalog is read by the Go check's own code, copied into
  `internal/apicheck/support.go`. The last release is read by
  `linetag.LastRelease`, the function the version bump and the Go check call.
  The marker is read by
  `ParseConventionalCommit` of `protocols/cli`, the function the version bump
  and the Go check call.

## Invariants

- A surface is a function of the file texts alone: no network, no dependency,
  no type-check (`TestReExportsAreFollowedInsideThePackage`).
- Without the `typescript` package, the check fails and starts no process
  (`TestWithoutTheTypeScriptPackageNothingIsRead`,
  `TestWithoutTheTypeScriptPackageTheCheckFails`).
- Two spellings of one type that the normalizations cover compare equal
  (`TestCompatibleChangesPass`).
- Removing `package.json` or an entry point does not end the comparison
  (`TestARemovedOrNarrowedManifestIsIncompatible`).
- A release the check cannot read gives a warning, not a failure no marker can
  clear (`TestAReleasedPackageTheCheckCannotReadWarns`).
- A change of written type never fails the task
  (`TestAChangedSignatureWarnsAndPassesAndARenamedParameterIsSilent`,
  `TestATypeChangeNextToARemovalFailsOnTheRemovalAlone`).
- A removal that a base or an `export *` the check cannot read may cover is a
  warning, and so is a declaration of another kind that keeps its uses
  (`TestComparisons`).
- A base is the one its own file names
  (`TestABaseIsResolvedInTheFileThatNamesIt`).
- The check never says an API it did not compare holds: an entry point it
  cannot read is a warning, and so is a package without one
  (`TestAPackageWithoutAnEntryPointIsNotCalledCompatible`,
  `TestACommonJSEntryPointWarnsAndIsNotCompared`).

## Consequences

- The check runs offline, in about one second per package on this repository,
  and adds no dependency.
- The check needs an installed workspace. A `validate` before `putnami install`
  fails with a message that names the command.
- The tag's sources are parsed by the parser of the working tree. A release
  that parser cannot read gives a warning.
- A break that only a type change carries passes with a warning. The author
  must read the warnings: the check no longer stops such a release.
- What the check does not see is listed in
  [doc/11-validate.md](../11-validate.md#limits). The main one: a type the
  source does not write is not compared.
- The extension holds a copy of the Go check's reading of the catalog. The two
  copies must stay equal.

## When to revisit

- When a reviewed waiver exists that clears a false positive without moving
  the version, a change of written type can fail again: the cost of a wrong
  failure is then a waiver, not a release.
- When the stable packages build with `isolatedDeclarations`, every exported
  type is written in source. Then `ts.transpileDeclaration` gives the
  declaration of each file without a type-check, and the unwritten-type limit
  goes away.
- When the catalog reader moves to a package both extensions can import,
  delete the copy.

## Rejected alternatives

- **Failing on every change of written type.** It was the first version. A
  text comparison cannot compute assignability, so it failed compatible edits
  and each one needed a marker and a release to clear.

- **`api-extractor`.** It needs the declarations of both revisions, so the tag
  must be checked out, installed and compiled on every run. Its report does
  not classify a change.
- **`tsc --declaration` on both revisions, then a diff.** Same cost, and a text
  diff of declarations reports every reordering and every renamed parameter.
- **A scanner written in Go.** It adds no process and no installed package,
  like the Go check. TypeScript syntax is not regular enough for it: a
  mis-scanned file would fail `validate` with no fix but a breaking marker.
- **Resolving `typescript` with bun.** Bun installs a package it does not find.
  The check would then reach the network and parse with a version the
  workspace did not choose.
