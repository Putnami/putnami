# @putnami/dart

`@putnami/dart` runs the packages of a Dart or Flutter repository as Putnami
projects: `putnami lint`, `putnami test` and `putnami build` run each
package's dart or flutter steps with Putnami's runner and cache, without
changing the build, and installs the Dart or Flutter SDK a package needs when
the machine has none. `workspace-install`, `deps-upgrade`, `run` and `serve`
resolve, upgrade, run and serve the packages.

It is a stack extension: a manifest, the Dart stack profile (`profile.json`,
with its `dart`, `flutter` and `dart-in-flutter` variants) and the entrypoint of the
stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The packages and
their edges come from the Dart detector of `putnami observe`.

See [the reference](doc/01-dart.md).
