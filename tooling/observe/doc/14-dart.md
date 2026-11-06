# How `putnami observe` reads a Dart or Flutter repository

A repository that holds a `pubspec.yaml` is a Dart repository, whether its
packages are Flutter apps, Flutter packages or plain Dart. The Dart detector
reads the `pubspec.yaml` files, the root pub workspace and the root
`melos.yaml`, and never runs `dart`, `flutter`, `pub` or `melos`. The change
analyses use what it finds to name the changed and dependent packages,
attribute tests and coverage, and diff `pubspec.lock`.

## Components

Each directory whose `pubspec.yaml` names a package is a component of
ecosystem `dart`, named by its pubspec `name`. The walk skips `node_modules`,
`Pods`, the directories whose name starts with `.`, such as `.dart_tool` and
the plugin symlinks of a Flutter app, and each `build` directory, the output
of `flutter build`, unless it holds a `pubspec.yaml` of its own. The copy of a
package that `dart run build_runner build --output build` writes holds one, so
it reads as a second `manifest` component of that name: write that output
elsewhere, or out of the checkout, before `putnami observe` runs.

| The package is | Source |
|---|---|
| a member of the root pub workspace: a path of the `workspace` list of the root `pubspec.yaml` (Dart 3.6 and later), matched as a glob | `workspace` |
| a directory the `packages` globs of the root `melos.yaml` match, less its `ignore` globs | `workspace` |
| any other package, such as a package's `example/` app | `manifest` |

A glob matches one directory name per `*` and any number of directories per
`**`. The root `pubspec.yaml` that declares a `workspace`, or sits beside a
`melos.yaml`, is the workspace root and no component, so a file at the root,
such as the root `pubspec.lock`, belongs to none. melos 7 reads its packages
from the pub workspace and its configuration from the `melos` key of the root
`pubspec.yaml`; melos 6 and earlier read `melos.yaml`. A root `pubspec.yaml`
of a repository that is neither is a component of source `manifest`, like any
other package.

Each finding is a warning:

| Code | Meaning |
|---|---|
| `observe.dart_pubspec_invalid` | A `pubspec.yaml` does not read or names no package, so it is no component; or the root `workspace` is not a list of paths. |
| `observe.dart_melos_invalid` | `melos.yaml` does not read, or its `packages` or `ignore` is not a list. |
| `observe.dart_pattern_invalid` | A `workspace`, `packages` or `ignore` entry leaves the repository or is no valid glob, and is ignored. |
| `observe.dart_walk_bounded` | The walk stopped after 200000 entries, or could not read the repository. |

The YAML reader reads the block and flow styles, quoted and block scalars,
comments, tags and anchors. An alias (`*name`) is not read, so a
`pubspec.yaml` that holds one is `observe.dart_pubspec_invalid`.

## Edges

A component depends on another when an entry of its `dependencies`,
`dev_dependencies` or `dependency_overrides` gives a `path` that resolves,
from the directory of its `pubspec.yaml`, to the other's directory:

```yaml
dependencies:
  pricing:
    path: ../../packages/pricing
```

Inside a pub workspace or a melos repository, which link their packages to
each other, an entry that names another of their packages with no source of
its own, such as `pricing: ^1.0.0`, is an edge too. A `git`, `sdk` or
`hosted` source, a path outside the repository or to no component, and the
package itself are no edge.

For example, when the Flutter app `apps/mobile` depends on `packages/pricing`
and `packages/ui`, a change to `pricing` reads:

```text
  components  1 changed, 1 dependent, 1 unaffected
    changed   packages/pricing (pricing) [dart, workspace]
    dependent apps/mobile (mobile) [dart, workspace], depends on packages/pricing, packages/ui
```

## Reports

Run each package's tests with coverage and a JUnit report, with the
[junitreport](https://pub.dev/packages/junitreport) package activated
(`dart pub global activate junitreport`):

```bash
melos exec --dir-exists=test -- "flutter test --machine --coverage | tojunit --output test-results.xml"
```

Without melos, run the same pipe in each package's directory; a plain Dart
package can run `dart test --reporter json | tojunit --output
test-results.xml`. `putnami observe` finds `test-results.xml` and
`coverage/lcov.info` by their content, as any report.

`flutter test --coverage` writes the paths of `coverage/lcov.info` relative to
the package, such as `SF:lib/src/price.dart`: they are read first from the
directory of the package that holds the report, then from the repository
root. An absolute path, which `dart run coverage:test_with_coverage` writes,
is read under the repository, or from another checkout by its trailing
segments.

tojunit writes no file for a test case: its class name is the test file's
path without `_test.dart`, each `/` written as a `.`. `test/price_test.dart`
is `test.price`; the absolute path `flutter test --machine` gives is
`.home.runner.work.shop.shop.packages.pricing.test.price`. The case lands on
the package that holds the longest tail of that path that names a file, from
the report's package or from the root, else on the package that holds the
report. tojunit also writes each `-` of the path as `_`, so a test in a
directory whose name holds a `-` lands on the package of its report.
`tojunit --base <directory>` strips that directory from the paths, which
gives class names relative to the package.

## Lockfiles

`pubspec.lock` is a lockfile of ecosystem `pub`: each entry of its `packages`
gives its name and version. A package of source `path` is the repository's
own code, and one of source `sdk`, such as `flutter` and `flutter_test`, comes
with the SDK: neither is a dependency. A `git` package gives the version its
`pubspec.yaml` states, so a change of its resolved commit alone is no
dependency change.

In a pub workspace, the members have no lockfile of their own: the root
`pubspec.lock` pins the packages of every member, and its change names no
component. In a melos 6 repository, each package that commits its
`pubspec.lock` has its change named on that package. A `pkg:pub` package URL
of an SBOM matches a `pub` dependency change.
