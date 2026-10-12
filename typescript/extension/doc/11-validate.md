# Validate

**Command:** `putnami validate [project]`

For every TypeScript package the support catalog lists as stable, `validate`
compares the public API with the last release of the project's version line.
It fails when something importers use is gone and no commit since that release
declares a breaking change.

The breaking-change marker decides the version: it is what makes
`putnami version` advance the line past a feature release. The check does not
decide whether a change is breaking. It catches a forgotten marker, in the
cases where no doubt is possible:

- **An error** is a change that breaks an importer whatever the types are,
  such as a removed export. It fails `validate` without a marker.
- **A warning** is a change of written type, such as a signature. It may or
  may not break an importer, and the check has no type information to tell.
  It is reported and never fails. You decide, and you add the marker when
  importers must change their code.

The split is deliberate. A wrong failure can only be cleared with a marker,
and a marker forces a release that announces a break nobody suffers. A missed
break only leaves the package where it was before the check existed.

**Activation:** Any project containing `package.json`. Other extensions
contribute their own steps to the same `validate` command.

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

A project is looked up as a `protocol` subject when it carries the `protocol`
tag, and as a `package` subject otherwise, under its project name. The tags
are the ones the CLI resolves: the project's own `tags` in `putnami.json`, else
the tags a workspace provider reports, else the tags of its scopes. An authored
`"tags": []` blocks the other two. The CLI passes the resolved tags to the
check in the job context, as `project.tags`. The check reads no tag from a
`putnami.json`, so it classifies a project as the CLI does.

When the job context carries no `project.tags`, the check reads the catalog
both as a package and as a protocol. It fails and names the member only when
the two readings disagree on whether the API is checked. A workspace without a
catalog reads no tag, so the check runs there with any context.

The version bump and the
[Go extension's check](../../../go/extension/doc/validate.md) read the catalog,
the last release and the marker by the same rules.

## What it compares

1. The baseline is the last release of the project's version line: the line of
   the nearest ancestor scope that declares one, else the root line
   `v{version}`. The last release is the line's highest release tag that HEAD
   reaches. A release tag carries a `MAJOR.MINOR.PATCH` version: a tag such as
   `ts/vnext-experiment` or `ts/v1.0.0-rc.1` is never the baseline, and an
   older release that a merged branch carries never replaces a later one. The
   version bump reads the same release. A line with no such tag has no released
   API, so the check passes and says so.
2. The released package is the project directory at the tag. When that
   directory holds no `package.json` at the tag, the check looks for the one
   `package.json` of the tag that declares the same `name`, and compares with
   its directory: a package that moved keeps its baseline. When no directory of
   the tag declares the name, nothing is compared. That is a warning for a
   package the catalog lists as stable, and an information line in a workspace
   without a catalog. When more than one directory declares it, the check
   cannot tell which one released: it warns and names the directories.
3. The public API is what the package's entry points export. An entry point
   is a subpath of `exports` in `package.json`, such as `.` or `./hooks`. Its
   conditions, such as `import`, `types` or `browser`, each name a file, and
   the subpath exports what any of them exports. A name is gone only when no
   condition of the subpath exports it now, so moving a subpath from one
   target to conditions, or back, changes nothing. A package without `exports`
   declares `.` with `main`, `module` and `types`. A subpath pattern such as
   `"./plugins/*": "./src/plugins/*.ts"` gives one entry point per matching
   file, except a subpath that a more specific key maps, `null` included:
   `"./internal/*": null` keeps `./internal/helper` out. A target without an
   extension is resolved the way Node and TypeScript resolve it:
   `./src/index` names `src/index.ts`, and `./src` names `src/index.ts`.
4. Each entry point is read from source, at the tag and in the working tree,
   uncommitted edits included. The check follows `export * from`,
   `export type * from`, `export * as name from` and `export { name } from`
   into the other files of the package. A specifier that ends in `.js` names
   the `.ts` file of the same name, and an alias that starts with `#` is
   resolved through the `imports` of `package.json`. It parses with the
   TypeScript parser the workspace installs. Nothing is compiled or
   type-checked, and no dependency is read.
5. A declaration the package does not export is compared too, when the
   written types of an exported one name it: the interface a function returns,
   the type of a parameter or of a member, the base of a class or of an
   interface. The check follows such names through the files of the package,
   level after level, and reads each declaration once.
6. The working tree's files are the ones git tracks, plus the untracked files
   git does not ignore. The check reads no test, at the tag or now: no file
   named `*.test.*` or `*.spec.*`, and nothing under the project's `test`
   directory. It reads nothing under a directory whose name starts with `.`,
   or under `node_modules`, `out`, `dist` or `vendor`, the directories the
   CLI's file walk never enters. An entry point that names such a file is not
   compared.

## Errors: what fails without a marker

| Change | Example message |
|---|---|
| An entry point removed, or pointed at a file that is not source | `@example/lib/hooks: entry point removed` |
| A file the tag held and the working tree does not, while `package.json` still names it | `@example/lib/legacy: entry point names src/legacy.ts, which the release held and the working tree does not` |
| An exported function, class, interface, type, enum, variable or namespace removed or renamed | `function greet removed` |
| A name re-exported from another package removed | `re-export z removed` |
| A member removed from an exported class, interface, object type or enum, or a member it inherited from a base the check reads | `method Client.send removed` |
| A name removed from a namespace, also when the namespace merges with a class, a function or an enum, or is the module of `export * as` | `namespace member Client.defaults removed` |
| A value that becomes a type only, or a type that becomes a value only | `interface Shape is now a variable` |
| A class or an enum that becomes another kind of declaration | `class make is now a function` |
| A class that becomes abstract | `class Client is now abstract` |
| An enum that becomes a `const enum` | `enum Level is now a const enum` |
| A class, an enum, a function or a variable now exported with `export type` | `class Client is now exported as a type only` |
| A member or a constructor that becomes `protected` or `private` | `constructor of Client is now private` |
| A property that can no longer be written: `readonly` added, or the setter removed | `property Client.url is now read-only` |
| A constructor that took parameters removed | `constructor of Client removed` |

A member that moves to a base class or a base interface is not removed, when
the check reads that base. A class also has the members of an interface of
the same name that merges with it, and the constructor of its nearest base
class when it declares none: a constructor that moves to a base is kept when
the base declares it with the same visibility. The base is resolved in the
file that names it, so another declaration of the same name elsewhere is
never taken for it.

The members a class or an interface inherited at the tag count as its own. A
class that drops `extends Base` removes the members of `Base` that it does
not declare now.

## Warnings: changes of written type

Each one is a warning diagnostic with the code `api-compat-type-change`. It
never fails the task.

| Change | Example message |
|---|---|
| A function signature: parameters, return type, `this` parameter, type predicate | `function greet changed from (string) => string to (string, boolean) => string` |
| An overload removed, changed or moved; overloads are compared in declaration order | `function parse changed overload 1 from (string) => string to (number) => number` |
| The written type of a variable or of a member | `variable limit changed type from number to string` |
| A type alias | `type Mode changed from "a" \| "b" to "a"` |
| Type parameters: removed, constrained differently, added without a default, a default removed or changed | `interface Box changed its type parameters from <$0 = string> to <$0>` |
| A heritage clause removed | `class Client no longer extends Base` |
| An interface that gains a base with a required member, or a base the check cannot read | `interface Config now extends Base, which adds the required member id: every value of the type built outside the package must have it` |
| A required member added to an interface or an object type | `property Config.name added as required: every value of the type built outside the package must have it` |
| A member that becomes required, optional or abstract; an abstract member added | `property Config.host is now required` |
| A constructor with required parameters added to a class that had none and extends nothing | `constructor of Box added with required parameters` |
| A method that becomes a property, or the reverse | `method Client.send is now a property` |
| A function that becomes a variable, or the reverse; an interface that becomes a type alias, or the reverse | `function handler is now a variable` |
| The value of an enum member | `enum member Level.Info changed value from 1 to 0` |
| A member removed from a class or an interface whose base the check cannot read: a base of another package, a base named through a namespace import, a mixin call | `method Client.close removed, unless a base the check cannot read declares it` |
| An `export *` the check cannot follow, at the tag or now; an export removed while the working tree has one | `export * from "dep" is not compared: the check cannot read the module it names` |
| The members of `export * as name` whose module the check cannot read whole | `the members of namespace Util are not compared: the check cannot read the whole module it names` |
| Any change, a removed member included, in a declaration the package does not export and that an exported one names | `property Inner.port removed` |

The last row is a warning on purpose. Importers have no name for a type the
package does not export, so they rarely depend on all of it, and the check
cannot tell which part they use.

Once a commit since the tag declares a breaking change, these are listed as
information.

## What is not reported

Additions: a new entry point, export, class member, optional member, enum
member at the end, and a new overload after the existing ones. These changes
are not reported either:

- A new optional or rest parameter at the end of a signature.
- A parameter that becomes optional or gains a default value.
- A renamed parameter or type parameter: signatures are compared by their
  types and by type parameter position, never by name.
- Another spelling of one type: `Array<T>` and `T[]`, either quote style of a
  string literal, and the members of a union or of an object type in another
  order.
- A private member, written with `private` or `#`, added, changed or removed,
  and a member that becomes public.
- A type parameter that gains a default, and a property that gains a setter.
- An interface rewritten as an alias of an object type with the same members,
  and a function rewritten as a constant that holds an arrow function.
- A source file that moves, when the entry point still exports the same names.

## The breaking-change marker

The check reads the commits from the tag to HEAD that touch the project
directory. For a package that moved since the tag, it also reads the commits
that touch the directory it had at the tag. When one of them is a conventional
commit that declares a breaking change, every error passes and is listed as
information:

```text
feat!: drop the greeting
```

```text
refactor: drop the greeting

BREAKING CHANGE: greet is gone; use wave.
```

The check and the version bump read a message with one function
(`ParseConventionalCommit`, see
[its reference](../../../protocols/cli/doc/07-conventional-commit.md)). A
message counts exactly when the version bump counts it: a subject of the form
`type(scope)!: summary`, or `type: summary` with a body line that starts with
`BREAKING CHANGE:` or its synonym `BREAKING-CHANGE:`. A marker on a commit that
does not touch the project does not count. An uncommitted change has no marker
yet: commit it with one.

A squash merge keeps only the pull request title and body. Put the marker in
the pull request title as well, for example `feat(lib)!: drop greet`, so the
commit on the main branch declares the break.

## Failure output

Each error is one error diagnostic with the code `api-compat`, naming the
package and the symbol:

```text
@example/lib: function greet removed since ts/v0.4.0; declare the breaking change with "!" or a BREAKING CHANGE: footer
```

The package is named as an importer writes it: `@example/lib` for the root
entry point and `@example/lib/hooks` for a subpath. A changed declaration also
carries its file and line. Fix the code, or keep the change and commit it with
the marker. Under a marker, each error is information that ends with "a commit
since ts/v0.4.0 declares the breaking change". The report names no commit: a
branch and its squash merge that declare the same break give the same verdict.

The task's data holds `compared`, `tag`, `entryPoints` (the number of subpaths
at the tag), `comparedEntryPoints` (the number whose exports were compared),
`incompatible` (the number of errors), `typeChanges` (the number of warnings),
`notCompared` (the number of subpaths the check could not read) and, when a
commit declares a breaking change, `breakingDeclared`. A failure reports the
`api-incompatible` metric.

A passing check says how much it compared: "5 entry points keep every export
and member of ts/v0.4.0; 1 other is still declared and has no exports the
check reads; 2 changes of written type are reported and do not fail the
check". When no entry point has exports the check reads, for example a package
of stylesheets, it says so and `compared` is `false`.

## When the check cannot compare

The task passes without comparing, with a warning diagnostic coded
`api-compat-not-compared`, when no marker could fix what stops it:

| Case | Result |
|---|---|
| The workspace is not in a git repository | a warning |
| The clone is shallow | a warning: fetch the full history, for example with `fetch-depth: 0` on GitHub Actions |
| No release tag of the line is reachable while the repository has other tags | a warning: fetch the line's tags |
| The project's `package.json` at the tag does not parse, is not a regular file or is over 1 MiB | a warning |
| A source file at the tag does not parse with the installed TypeScript | a warning: update the `typescript` package |
| A condition of an entry point names a source file git does not hold now, such as build output, or a path without an extension that resolves to no source file | a warning for that entry point; the others are compared |
| No condition of an entry point names, at the tag, a file the tag holds and the check reads as a module: build output, a script or a CommonJS module, with no `import` and no `export` or with `export =` | a warning for that entry point; the others are compared |
| The `package.json` at the tag declares no entry point | a warning |
| The catalog lists the package as stable and no `package.json` of the tag declares its name | a warning |
| More than one `package.json` of the tag declares the package's name | a warning that names their directories |

A source file at the tag that does not parse is a warning on purpose, where
the [Go extension's check](../../../go/extension/doc/validate.md) fails. Go
source that built at a release parses with every later `go/parser`. A
TypeScript release may use syntax the installed parser does not read, older
or newer, and no change to the working tree makes a released file parse:
failing would block every run until the next release.

These cases fail the task, because the fix is in the working tree:

| Case | Result |
|---|---|
| The `package.json` of the working tree does not parse | the task fails |
| A source file an entry point reaches does not parse | the task fails, naming the file and the line |
| The `typescript` package does not resolve from the project | the task fails: run `putnami install` |

Removing `package.json`, or an entry of its `exports`, does not end the
comparison: every entry point the tag declared and the working tree does not
is reported as removed.

## Caching

The task is cached. Its key holds what the verdict reads:

- the project's `package.json` and its TypeScript and JavaScript files, the
  files the check reads (see step 6 of "What it compares");
- the root `putnami.support.json`;
- the root `package.json` and bun lockfile, which select the `typescript`
  package that parses both sides;
- the project's resolved tags, which the job context carries as
  `project.tags`;
- the project's release baseline, which the CLI reads from git before the task
  runs (the `releaseBaseline` runtime input): the repository state, the line's
  tag pattern, the last tag of the line HEAD reaches, the tree that tag holds
  at the project directory, and whether a commit since that tag that touches
  the project declares a breaking change.

The key names no commit. A pull request and the commit its squash merge puts
on the main branch share the verdict when they hold one tree and their
messages declare the same break: put the marker in the pull request title. A
new last release, a moved release tag, a commit that adds or drops a marker,
or a shallow clone changes the key. The
[Go extension's check](../../../go/extension/doc/validate.md#caching) is keyed
the same way.

For a package that moved since its release, the key does not hold the
commits that touch only its former directory: a marker on such a commit
reaches a cached verdict once another input of the key moves.

## Limits

- **Warnings are not verdicts.** A change of written type is reported with no
  type information. Many warnings are compatible changes: a type replaced by
  an equivalent alias, a parameter widened from `Dog` to its base `Animal`, a
  required member added to a type only the package builds. Some are real
  breaks. Read each one and add the marker when importers must change.
- **Breaks the check does not see.** These pass without an error or a
  warning:
  - a change to a type the source does not write: an inferred return type, a
    variable or a parameter without an annotation, a default export that is an
    object literal, a constant that holds a class expression;
  - a member that a base class or interface from another package stops
    providing;
  - a parameter that becomes optional in an interface, an object type or a
    call signature, which a class outside the package implements;
  - a change inside a namespace's exports, beyond a removed name;
  - decorators, `declare module` and `declare global` augmentations, and a
    `let` that becomes a `const`.
- **Declarations held by their existence only.** The check fails on their
  removal and reports nothing else: a name re-exported from another package; a
  name whose declaration is in a file the package does not hold, such as
  generated code git ignores; the names of `export import name = …` and of a
  destructured export.
- **Names the check does not follow.** A qualified name such as `ns.Type`, a
  `typeof value` type, and a type another package declares.
- **A member moved to a base the check cannot read** is a warning, not an
  error: the check cannot tell whether the base declares it.
- **CommonJS.** A file with no `import` and no `export`, or with `export =`,
  is not compared; the check warns.
- **Declaration files.** A `.d.ts` module without an export list exports
  every top-level declaration, and the check reads it that way. A `.d.ts`
  script, with no `import` and no `export`, is not compared.
- **Conditions are one entry point.** The exports of a subpath are what any
  of its conditions exports. A name that one condition drops while another
  still exports it passes, though an importer that resolves the first
  condition loses it.
- **A moved package** is found by its `name`. A package that moved and was
  renamed in one release has no baseline; the check warns.
- **Commands.** The `bin` entries of a package are not part of the check.

The choice of a parsed, source-level comparison over `api-extractor` is
recorded in [ADR 0004](adr/0004-the-api-check-parses-source-and-does-not-type-check.md).
