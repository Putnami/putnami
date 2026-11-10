# Run a Dart or Flutter repository with Putnami

`@putnami/dart` turns each package of a Dart or Flutter repository into a
Putnami project and runs its lint, test and build with the dart and flutter
tools the team already uses. It reads the repository the way `putnami
observe` does, and installs the SDK a package needs when the machine has
none.

## Adopt it

`@putnami/dart` is not published yet: until its first release, `putnami init
--extension dart` stops at the extension install, after it has written the
workspace files, and a workspace loads the extension from a source checkout.

1. At the root of the repository, run:

   ```bash
   putnami init --extension dart
   ```

   It writes `putnami.workspace.json` with `@putnami/dart` declared, installs
   the extension, and runs `putnami projects sync`, because the repository
   holds a `pubspec.yaml`. A repository of several languages takes a list, such as
   `--extension dart,go`, and `putnami init` without `--extension`
   proposes the languages it finds. In a workspace that already exists,
   declare the extension in `putnami.workspace.json` and run
   `putnami projects sync`:

   ```json
   { "extensions": ["@putnami/dart"] }
   ```

2. Each directory whose `pubspec.yaml` names a
   package joins the workspace's `includes`; the root of a pub workspace does
   not. No `putnami.json` is needed: the extension names the project by its
   pubspec name, adds an edge to each package it depends on by path or through
   the root pub workspace or melos repository, and tags it `dart`.
3. Commit the `pubspec.lock` of each pub workspace and standalone package: the
   tasks resolve with `--enforce-lockfile`, so they never write the lockfile
   they read, and a package without one fails with pub's own message.
4. Run `putnami lint,test,build`. A machine with no Dart or Flutter SDK
   needs nothing more: see [Install the SDK](#install-the-sdk).

## Dart and Flutter packages

A package whose `pubspec.yaml` depends on the Flutter SDK (`sdk: flutter`, in
`dependencies` or `dev_dependencies`) or states `environment.flutter` runs the
`flutter` variant of the profile. A pure Dart package that a pub workspace
holding a Flutter package lists runs the `dart-in-flutter` variant: only
flutter resolves that workspace, so the package resolves with
`flutter pub get` and runs `dart`. Any other package is a pure Dart package
and runs the `dart` variant, the profile itself, a pure Dart melos package
included, since it resolves on its own.

A package that a pub workspace lists, at the workspace root or below it, has
that workspace's directory for stack root: pub resolves it there, with one
`pubspec.lock`. The root of a pub workspace below the workspace root is an
input-only project, whose selectors answer its `pubspec.yaml` and
`pubspec.lock`; each member depends on it. Any other package, a melos package
included, is its own stack root.

## What each task runs

Each task runs in the package's directory, with no shell. An `install` step
first resolves the package's stack root with `pub get --enforce-lockfile`,
with `flutter` when the root holds a Flutter package, unless the selected SDK
resolved it last: a marker in `.dart_tool/` records the SDK program and the
version it answers, the pubspecs, `pubspec.lock` and `package_config.json` of
that resolution, so a resolution another SDK or an editor wrote since, or a
shim that now selects another SDK, runs `pub get` again. A root with no
`pubspec.lock` fails: no task writes the lockfile it reads. The input-only root
of a pub workspace below the workspace root resolves nothing itself: its
members' install steps resolve it with their SDK. An offline run resolves nothing there: `workspace-install`
resolved every root from the pub cache before.

| Task | Dart package | Flutter package |
|---|---|---|
| `lint` | `dart analyze lib` and `dart analyze bin`, for each that exists | The same |
| `test` | `dart test --coverage={output}/coverage --file-reporter=json:{output}/tests.json`, then `format_coverage` into `{output}/lcov.info` and `tojunit` | `flutter test --no-pub --coverage --file-reporter=json:{output}/tests.json`, then `tojunit` |
| `build` | `dart compile exe bin/<name>.dart -o {output}/<name>` for each `bin/*.dart` | `flutter build <platform>` for each platform of an app |

A `dart-in-flutter` package runs the Dart package's steps. A step's `dart` is
the one beside the selected `flutter` for a Flutter or `dart-in-flutter`
package, whatever comes first on `PATH`.

`format_coverage` and `tojunit` are report steps: they run after the test
runner whatever its outcome, so a failed case names the failure. `tojunit`
strips the package directory from each test's class name. The test task reads
the JUnit report `tojunit` writes and the LCOV report, `coverage/lcov.info`
for a Flutter package, into the test and coverage summary. A failed case, a
step that exits non-zero, or a missing report fails the task; a package
without a `test/` directory fails its test task, since `dart test` and
`flutter test` find nothing to run.

`flutter build` resolves again, keeping the locked versions, and writes the
release plugin registrant. It may rewrite `pubspec.lock` when the lockfile is
stale in a way `--enforce-lockfile` does not check, such as its `sdks` after
an `environment` change, a lockfile with no content hashes, or a
`pub.dartlang.org` URL: commit the lockfile a plain `pub get` writes.

A Flutter package with `lib/main.dart` is an app. Its build runs one
`flutter build` per platform directory it holds that the host builds:

| Directory | Target | Hosts |
|---|---|---|
| `android/` | `apk` | every host |
| `web/` | `web` | every host |
| `linux/` | `linux` | Linux |
| `macos/` | `macos` | macOS |
| `windows/` | `windows` | Windows |

A Flutter package that is no app builds nothing. iOS needs code signing, so
it is no default target: add a step, as shown below. The release APK,
`build/app/outputs/flutter-apk/app-release.apk`, is copied into the task's
output, so a cache hit restores it. The web and desktop bundles are
directories that stay under `build/`: a cache hit does not restore them.

The packages of one pub workspace run one task at a time, since they share
its `.dart_tool/` and its `pubspec.lock`. A task of a package waits for the
same task of the packages it depends on.

## Install the SDK

When the `dart` or `flutter` on `PATH` is missing, or it or the Dart a Flutter
SDK bundles is outside the constraints its package states, a task installs the
newest release the extension pins that the constraints admit, under
`tools/dart` of the Putnami home, and runs it
([ADR 0001](https://github.com/Putnami/putnami/blob/main/dart/extension/doc/adr/0001-the-sdk-installs-from-the-publishers-archives.md),
[ADR 0003](https://github.com/Putnami/putnami/blob/main/dart/extension/doc/adr/0003-the-extension-pins-the-sdk-releases-it-installs.md)):

| SDK | Pinned releases | Archive |
|---|---|---|
| Dart | The newest patch of each stable minor from 2.19, with the SHA-256 of each platform's archive | The dart-archive on `storage.googleapis.com` |
| Flutter | The newest stable release of each minor from 3.10 on each system; the release must also bundle a Dart in `environment.sdk` | The Flutter release bucket on `storage.googleapis.com` |

The pins live in `sdkreleases.json`, which `scripts/sdk-releases.sh` writes
from the publishers' release lists, so the same extension release installs
the same SDK on every machine; a pin moves with the next extension release.
The archive is checked against the pinned SHA-256 before it is unpacked, and
later tasks reuse the install. A constraint that admits only a patch the
extension does not pin, such as `sdk: 3.4.0`, fails: put that SDK on `PATH`.
An SDK on `PATH` that satisfies the constraint runs, and nothing is
installed. An offline run installs nothing. A Flutter archive weighs 1.5 to
2.5 GB and unpacks with the host's `tar`, which needs `xz` on Linux, without
keeping the archive's owner; Flutter itself needs `git`. Flutter publishes no
archive for Linux on arm64, where `flutter` must be on `PATH`.

The test task runs `tojunit` and `format_coverage`. For each one its `PATH`
lacks, it activates `junitreport` 2.0.2 or `coverage` 1.x with the task's
`dart`, the one beside the selected `flutter` for a package that resolves with
Flutter, into a pub cache of that Dart SDK version under the Putnami home, and
runs it from there with that `dart`: the machine's pub cache is untouched. A team that has activated them itself, with
`~/.pub-cache/bin` on `PATH`, runs its own.

## The SDK version check

Before its steps, each task compares the SDK versions on the machine with the
constraints the package's `pubspec.yaml` states, then the root's:

| Package | Command | Constraints |
|---|---|---|
| Dart | `dart --version` | `environment.sdk` |
| Flutter | `flutter --version` | `environment.flutter`, and `environment.sdk` for the Dart SDK the Flutter SDK bundles |

The constraints read as pub reads them: a null-safe `environment.sdk` whose
upper bound is `<3.0.0` admits Dart 3, and, below language version 3.9,
`environment.flutter` is a lower bound, its upper bound ignored. From an
`environment.sdk` of 3.9, `environment.flutter` reads as stated.

| Case | Result |
|---|---|
| The version is inside the constraint, such as 3.7.2 for `^3.7.0` | The task runs |
| It is outside | The task fails with `stacks.toolchain_mismatch`; set `toolchain.check` to `warn` to run anyway |
| The command is not on `PATH` | The task fails with `stacks.toolchain_missing` |
| No pubspec states any of the constraints | The warning `stacks.toolchain_unpinned`: the cache key carries no SDK version |

The cache key carries the constraints, so two machines with versions inside
them share entries. A Flutter package that states only `environment.sdk`, as
`flutter create` writes it, keys and checks its Dart SDK alone.

## What reaches the cache key

A task's key covers the package's files its task reads, the workspace-root
`pubspec.yaml`, `analysis_options.yaml` and `pubspec.lock`, the package's
`putnami.json`, its variant, the expected SDK version, and the key of the
same task of each package it depends on.

| Input set | Patterns |
|---|---|
| `code` | `lib/**`, `bin/**` |
| `tests` | `test/**`, `dart_test.yaml` |
| `assets` | `assets/**`, `fonts/**`, `l10n.yaml` |
| `manifests` | `pubspec.yaml`, `pubspec_overrides.yaml`, `analysis_options.yaml`, and the root's `pubspec.yaml` and `analysis_options.yaml` |
| `lockfiles` | `pubspec.lock`, and the root's `pubspec.lock` |
| `platforms` | The sources of the `android/`, `ios/`, `macos/`, `web/`, `linux/` and `windows/` directories, without the files `flutter pub get` and `flutter build` generate there: the ephemeral directories and each plugin registrant |

| Task | Input sets |
|---|---|
| `lint` | `code`, `manifests`, `lockfiles` |
| `test` | `code`, `tests`, `assets`, `manifests`, `lockfiles` |
| `build` | `code`, `manifests`, `lockfiles`; a Flutter package adds `assets` and `platforms` |

`lint` reads no test file, so a change under `test/` reruns test alone. The
build task also keys the host system, since a Flutter app's targets depend on
it.

## Override the profile

A package overrides any value of its profile in its own `putnami.json`, under
`options["@putnami/dart"]`, on top of its variant. An object merges; any
other value, such as a list of steps or patterns, replaces the profile's
whole. The overrides a team uses most:

```json
{
  "options": {
    "@putnami/dart": {
      "tasks": {
        "lint": {
          "steps": [["dart", "analyze", "--fatal-infos", "{target}"]],
          "targets": [{ "name": "lib", "when": ["lib"] }, { "name": "test", "when": ["test"] }],
          "inputs": ["code", "tests", "manifests", "lockfiles"]
        },
        "test": { "steps": [
          ["flutter", "test", "--no-pub", "--coverage", "--dart-define=ENV=ci", "--file-reporter=json:{output}/tests.json"]
        ] },
        "build": { "steps": [["flutter", "build", "ios", "--no-codesign"]], "targets": [] }
      },
      "inputs": { "assets": ["assets/**", "fonts/**", "l10n.yaml", "shaders/**", "images/**"] },
      "toolchain": { "check": "warn" }
    }
  }
}
```

The lint override above also analyzes `test/`, so it adds `tests` to its
inputs: its key then follows the test files. A steps override keeps the
profile's report steps and reports: the test step must still write
`{output}/tests.json` for `tojunit`, and the LCOV report, or the override also
replaces `tasks.test.reportSteps` and `tasks.test.reports`. The install step
resolves the stack root before every task, so no step needs `pub get`. An override
fails the task with `stacks.project_config_invalid` when it names an unknown
member, changes `id`, `markers` or `variants`, adds a workspace-root input the
extension does not declare, names a target whose paths are in none of the
task's inputs, or adds `hosts` to a task other than build, which alone keys
the host system. The design of the stack core is in
[its design note](https://github.com/Putnami/putnami/blob/main/tooling/stacks/doc/01-design.md).

## Workspace commands

| Command | What it runs |
|---|---|
| `putnami workspace-fetch` | `pub get --enforce-lockfile` at each stack root, which downloads the locked packages into the pub cache and runs none of their code. A hosted run calls it with the network, before any repository process. A root with no `pubspec.lock` fails it. |
| `putnami workspace-install` | `pub get --enforce-lockfile` at each stack root, with `flutter` when the root holds a Flutter package, `--offline` in an offline run. A root the selected SDK resolved last, with the same pubspecs, `pubspec.lock` and `package_config.json`, is current and skipped; `--force` resolves it anyway. A root with no `pubspec.lock` yet resolves one. A root whose SDK the machine lacks in an offline run is left out with a warning. A change to `pubspec.lock` at the workspace root makes the next `putnami` command run it again. |
| `putnami deps-upgrade` | `pub upgrade` at each stack root. It needs the network. |
| `putnami run` | `dart run` in a Dart command-line package, with `--entrypoint` and `--args`, and forwards its exit code. A Flutter app runs on a device with `flutter run`, outside Putnami. |
| `putnami serve` | A Dart server: `dart run` on `bin/server.dart`, or `--entrypoint`, followed by the words of `--args`, or the package's executable, with `PORT` set. A Flutter app with `web/`: `flutter run -d web-server --web-port <port> --web-hostname 0.0.0.0`. It reports ready once the port accepts connections; `--port 0` picks a free one. |
| `putnami lint --docs-links` | Checks every relative link and anchor of the package's `README.md` and `doc/`, on by default. |

On a hosted run, the engine hands `workspace-fetch` a read credential on an
inherited descriptor. The job reads that descriptor and closes it before it
starts any process, the SDK check included, so no `dart` or `flutter`
inherits it or finds the credential in its environment. The job then uses
nothing the descriptor held: the credential's hosts are the Putnami
registries, which serve no pub package, so no pub source can use it. `pub get`
reaches pub.dev, which needs no credential to download, and a hosted
repository a pubspec names with the tokens `dart pub token add` stored on the
machine. A descriptor the job cannot read fails it with `dart.pub_get_failed`
before any process starts.

A package with `bin/*.dart` or `lib/main.dart` is an application, which `run`
and `serve` accept; any other is a library.

## Package and publish an image

A Dart server application reaches a registry as an OCI image, a standard
container image. It is a pure Dart package, not a Flutter one, with a
`bin/*.dart`. Declare the channel in the project's `putnami.json`:

```json
{ "options": { "package": { "docker": true }, "publish": { "docker": true } } }
```

`putnami package` resolves the stack root as the other tasks do, then
assembles the image with no Docker daemon. `--docker-load` also loads it into
the local Docker daemon, and `--docker-tag` adds a tag. The image name is the
project name in lowercase. `putnami publish` pushes it to the registry that
`registries.oci.publish` of `putnami.workspace.json` declares, through the
shared publisher of the other stacks; `--docker-registry <registry>` overrides
that registry for one push. A library and a Flutter package package and
publish nothing: a Flutter app ships through a store.

The image holds one executable, compiled ahead of time for Linux from any
machine ([ADR 0002](https://github.com/Putnami/putnami/blob/main/dart/extension/doc/adr/0002-an-application-image-holds-an-aot-executable-on-a-glibc-base.md)):

1. The entrypoint is the one `options.package.entrypoint` or `--entrypoint`
   names, a Dart file of the package such as `bin/api.dart`, or the name of a
   `bin/` executable such as `api`. Without one, it is `bin/server.dart`,
   which `serve` starts, else `bin/<package>.dart`, which `dart run` starts,
   else the package's only `bin/*.dart`. A package with several and none of
   these fails with `dart.no_entrypoint`, which names them.
2. The selected `dart`, the one `build` runs, must be 3.8.0 or later: earlier
   releases cannot compile for another architecture, and fail with
   `dart.cross_compile_unsupported`. State `environment.sdk: ^3.8.0` so that a
   task installs a release that can.
3. `dart compile exe <entrypoint> -o <name> --target-os linux --target-arch
   <x64|arm64>` compiles the executable in the package directory, holding the
   lock of the stack root as the other tasks do, into a directory of its own.
   `--platform` selects `linux/amd64`, the default, or `linux/arm64`; another
   fails with `dart.platform_unsupported`. A target other than the machine's
   downloads its compiler and runtime from the dart-archive into `~/.dart` the
   first time.

| Path | Content |
|---|---|
| `/app/<name>` | The executable, named after its entrypoint file, the image's entrypoint |

The executable links the C library dynamically, so the base is
`gcr.io/distroless/cc-debian12:nonroot` pinned by digest, which holds glibc,
CA certificates and time zones and no shell. The image runs as the user
`65532`, the `nonroot` user of distroless, in `/app`, with `PORT` set and
exposed from `--port`, 8080 by default: the server reads `PORT` to listen, as it does under `serve`. A
project names another base with `--docker-base-image image@sha256:<digest>`; a
base with no digest fails with `dart.base_not_pinned`. The cache key of
`package` covers the package's build inputs, the `lib/` and `pubspec.yaml` of
each package it depends on, and the flags above.

The image holds no file but the executable: a server that reads files at run
time, such as a `public/` directory, names a base that holds them.

`putnami test --test` is not offered: `dart test` and `flutter test` fail a
run whose filter selects no test, so a filter that reaches the packages a
selected one depends on would fail them.

## Limits

- On Windows, `dart compile exe` writes the executable under the target's
  name, with no `.exe`.
- An offline task installs nothing: a hosted run needs `dart` or `flutter`,
  `tojunit` and `format_coverage` on `PATH` of its runner.
- Each package selects its SDK from its own pubspec, then its stack root's:
  the members of one pub workspace whose constraints admit no common SDK
  resolve with whichever installs first.
- No package reaches pub.dev through Putnami: publish it with
  `dart pub publish`.
- An image holds a pure Dart application only. A package with build hooks
  cannot be packaged, since `dart compile` refuses it, and a program that loads
  a native library through `dart:ffi` finds only what the base holds.
- An offline `package` for an architecture other than the machine's needs the
  target's compiler in `~/.dart` already.
- An arm64 image is assembled as an amd64 one is; no test runs one.
- melos scripts do not run: the tasks run the profile's dart and flutter
  steps.
