# Run Swift packages and Xcode projects with Putnami

`@putnami/swift` turns each Swift package and each Xcode project of a
repository into a Putnami project, and runs its lint, test and build with the
tools the team already uses: `swift` for a package, `xcodebuild` for an Xcode
project. It reads the repository the way `putnami observe` does. On a machine
with no Swift, or with a Swift outside the release a package states, a package
task installs that release under the Putnami home; it never installs Xcode.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/swift"] }
   ```

2. Run `putnami projects sync`. Each directory with a `Package.swift`, and each
   directory that holds an `.xcodeproj`, joins the workspace's `includes`. No
   `putnami.json` is needed: the extension names a package by its package
   name and an Xcode project by its first `.xcodeproj`, adds an edge to each
   local package it uses (`.package(path:)`, or the project's local package
   references), and tags it `swift`.
3. Run `putnami lint,test,build`.

A package's targets are not projects of their own: `swift` builds and tests a
package as a whole. For the exact target edges of a package, write its export
beside the manifest in CI, as `putnami observe` advises:
`swift package describe --type json > swiftpm.json`.

## What each task runs

Each task runs in the component's directory, with no shell. `{component}` is
the package name, or the Xcode project's name; `{output}` is the task's output
directory. `{runtime}` is the extension's own executable.

| Task | Package | Xcode project |
|---|---|---|
| `lint` | `{runtime} swift-format-lint Package.swift Sources` | `{runtime} swift-format-lint .` |
| `test` | `{runtime} swiftpm-test --output {output}` | `{runtime} xcodebuild test --scheme {component} --result-bundle {output}/Tests.xcresult --junit {output}/junit.xml --cobertura {output}/coverage.xml` |
| `build` | `swift build --force-resolved-versions` | `{runtime} xcodebuild build --scheme {component}` |

Every step resolves packages with the versions the committed
`Package.resolved` pins (`--force-resolved-versions`,
`-disableAutomaticPackageResolution`), so a run never rewrites one of its own
inputs. A component with remote packages commits its `Package.resolved`: the
package's own, or `<name>.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved`.

The runtime commands:

- `swift-format-lint` runs `swift format lint --strict` (Swift 6 and later)
  on the paths that exist, with the `.swift-format` of the component, else of
  the workspace root: the two files the task's key reads. Without one, it
  lints nothing and says so: swift-format's default style is no team's style.
- `swiftpm-test` runs `swift test --force-resolved-versions
  --enable-code-coverage --parallel --xunit-output {output}/xunit.xml`, with
  the arguments after `--`, then exports the coverage to LCOV: `llvm-cov
  export` of the package's test binary with `default.profdata`, kept to the
  files under the package outside `.build` and `Tests`. It runs `llvm-cov`
  from `PATH`, else through `xcrun`. A `swift test` that exits non-zero with
  a failed case lets the task read the failed cases; one with no failed case,
  a compile error for instance, fails the step. SwiftPM writes no coverage
  profile after a failed run, so such a run has no LCOV, and the task names
  the failed cases and the missing report.
- `xcodebuild` runs `xcodebuild test` or `build` on the scheme, with
  `CODE_SIGNING_ALLOWED=NO`. Without `--destination`, it reads the scheme's
  `PLATFORM_NAME`: a Mac scheme runs on `platform=macOS`; an iOS, tvOS,
  watchOS or visionOS scheme builds for the generic simulator and tests on the
  first available simulator of the newest runtime. A test writes its result
  bundle with code coverage, then, with `--junit` or `--cobertura`, converts
  it as `xcresult` does; a failed test leaves the verdict to the failed cases.
- `xcresult` converts the result bundle: a JUnit report from `xcrun
  xcresulttool get test-results tests` (Xcode 16 and later), one suite per
  `Bundle.Suite`, and a Cobertura report from `xcrun xccov` of the app
  targets' files under the project's directory. When `xcodebuild test`
  failed and no case did, it fails.

Coverage stays with its component: a local package the tests link reports
its own coverage, in its own task.

The test task reads the JUnit and coverage reports into its test and coverage
summary. A failed case, a step that fails, or a report that is missing fails
the task.

## Test filter and documentation links

`putnami test --test <filter>`, short `-t`, adds `--filter <filter>` to the
task's last step when that step is the `swiftpm-test` or `xcodebuild` command;
an override whose last step runs another program gets no filter. The
`swiftpm-test` command hands it to `swift test --filter`, a regular expression
over `Module.Class/method`; the `xcodebuild` command hands it to
`xcodebuild -only-testing:<filter>`, as `Target/Class/method`. The filter also
reaches the components the selected ones depend on: a package in which it
selects no test passes with empty reports. An Xcode project whose scheme has
no target the filter names fails, since `xcodebuild` refuses the selection.

`putnami lint` also checks every relative link and anchor of the component's
`README.md` files and `doc/` trees, and a broken one fails it.
`--docs-links=false` turns the check off.

## Install and upgrade dependencies

`putnami install` runs the `workspace-install` job of this extension over the
workspace's components, except the projects `disable.tags` excludes:

| Component | What it runs |
|---|---|
| Package | `swift package resolve --force-resolved-versions`, except when its last successful resolve used its current `Package.swift`, `Package@swift-*.swift` and `Package.resolved`; `--force` resolves it as well |
| Xcode project | `xcodebuild -resolvePackageDependencies -onlyUsePackageVersionsFromResolvedFile` |

With no Swift on the machine, a package installs its Swift as a task does,
and an Xcode project without Xcode warns and passes. With offline dependencies,
as in a hosted run, it resolves nothing ahead of time: `swift build` and
`xcodebuild` then resolve the pinned versions themselves, over the network the
runner allows.

There is no `workspace-fetch`: resolving a package runs its `Package.swift`,
and the packages it depends on, with no sandbox on Linux, so the extension
fetches nothing ahead of a hosted run.

`putnami upgrade` runs `swift package update` in each package, which rewrites
its `Package.resolved`. An Xcode project keeps its pins and is left out, so a
machine with no Xcode still upgrades its packages: update an Xcode project's
packages in Xcode. With offline dependencies, the upgrade fails.

## Run an application

A package is an application when its `Package.swift` declares an executable
product (`.executable(name:)`) or an executable target
(`.executableTarget(name:)`); any other package is a library, and `serve` and
`run` skip it. An Xcode project is neither served nor run.

| Command | What it runs |
|---|---|
| `putnami serve <project>` | `swift run --force-resolved-versions <executable>` in the package, with `PORT` set: `--port` defaults to 3000, and 0 selects a free port. The executable reads `PORT` to listen. The command reports the server ready once the port accepts a connection, so `putnami qualify` and compose can wait on it. There is no watcher. |
| `putnami run <project> --args "<arguments>"` | `swift run --force-resolved-versions <executable> -- <arguments>`, and exits with its exit code. |

Both resolve with the versions `Package.resolved` pins. The executable is the
package's only executable product, or, with none, its only executable target;
with several, the one named after the project, else the one `--entrypoint`
names.

## Package and publish an image

An application reaches a registry as an OCI image, a standard container image.
Declare the channel in the project's `putnami.json`:

```json
{ "options": { "package": { "docker": true }, "publish": { "docker": true } } }
```

`putnami package` assembles the image with no Docker daemon. `--docker-load`
also loads it into the local Docker daemon. `putnami publish` pushes it to the
registry that `registries.oci.publish` of `putnami.workspace.json` declares,
through the shared publisher of the other stacks. `--docker-registry
<registry>` overrides that registry for one push. A library or an Xcode
project packages and publishes nothing.

The image holds a static binary, built with the
[Static Linux SDK](https://www.swift.org/documentation/articles/static-linux-getting-started.html)
of the selected Swift, so the same command packages from macOS and from Linux
([ADR 0001](adr/0001-an-application-image-holds-a-static-binary.md)):

1. `swift --version` names the swift.org release, such as `6.4.2`. The
   extension pins the checksum of the SDK of each release from 6.4.0 to
   6.4.2; another release fails with `swift.static_sdk_unavailable` and names
   the pinned ones. Before 6.4, SwiftPM selects an SDK of the bundle
   whatever `--triple` names, so no earlier release is pinned. The Swift of Xcode cannot build with the SDK: on macOS, select
   a swift.org toolchain with swiftly or the swift.org installer.
2. When `swift sdk list` does not name the SDK, `swift sdk install <url>
   --checksum <pin>` installs it, once per machine: one install of a bundle
   runs at a time.
3. `swift build -c release --swift-sdk <bundle> --triple
   <arch>-swift-linux-musl --force-resolved-versions --product <executable>
   -Xlinker -s` builds the executable, stripped, with the pinned SDK and the
   versions `Package.resolved` pins, holding the lock of the package as the
   other tasks do. The
   executable is the one `serve` starts; `--entrypoint` names it among several.
   `--platform` selects `linux/amd64`, the default, or `linux/arm64`.

| Layer | Content |
|---|---|
| Resources | The `.resources` and `.bundle` directories SwiftPM writes beside the executable, under `/app`, where `Bundle.module` finds them. The job removes those of earlier builds first, so the image holds the bundles of its product only. Only a package with resources has this layer. |
| Executable | `/app/<executable>`, the image's entrypoint |

The image runs as the user `65532`, the `nonroot` user of distroless, in
`/app`, with `PORT=8080` set and exposed: the executable reads `PORT` to
listen, as it does under `serve`. The base is `gcr.io/distroless/static:nonroot`
pinned by digest, which holds CA certificates and time zones and no shell. A
project names another base with `--docker-base-image image@sha256:<digest>`; a
base with no digest fails with `swift.base_not_pinned`.

A Vapor application that serves files from `Public/` or renders templates from
`Resources/` reads them from its working directory, which the image does not
hold: declare them as target resources so that they reach `Bundle.module`.

## The toolchain check

Before its first step, each task compares the version on the machine with the
version the component states:

| Component | Machine version | Expected version |
|---|---|---|
| Package | `swift --version` (`Apple Swift version 6.0.2`) | `.swift-version` in the package |
| Xcode project | `xcodebuild -version` (`Xcode 16.1`) | `.xcode-version` in the project's directory |

A version with fewer components names the releases it prefixes: `6.0` matches
6.0.2. A mismatch fails the task with `stacks.toolchain_mismatch`; set
`toolchain.check` to `warn` to run anyway. A component that states no version
gets `stacks.toolchain_unpinned`, a warning, and its cache key carries no
toolchain version.

## Swift installed at task time

A package task whose `swift` is missing, or outside the release its
`.swift-version` states, installs that release, or 6.4.2 when the package
states none, and runs it. A `swift` on `PATH` that satisfies the statement
always runs, and nothing is installed.

- The install runs [swiftly](https://github.com/swiftlang/swiftly) 1.2.0,
  which the extension downloads and checks against the SHA-256 it pins for
  Linux (x86_64, aarch64) and macOS. It runs with a home, a toolchains
  directory and a gpg keyring of its own under
  `<Putnami home>/tools/swift`, so the machine's swiftly, toolchains and
  keyring stay untouched. On Linux, swiftly checks the toolchain's signature
  with `gpg`, and needs the system's CA certificates (`ca-certificates`), which
  the machine provides; it installs on the distributions swift.org publishes
  for: Ubuntu, Debian, Fedora, Amazon Linux and Red Hat UBI. On another one,
  the task fails with swiftly's reason. On macOS, the installed toolchain
  builds with the SDK of the Xcode Command Line Tools, which the machine
  provides.
- `.swift-version` names a release: `6`, `6.4` or `6.4.2`, as swiftly takes
  it; `6.4` installs the newest 6.4 patch. Any other statement, such as a
  snapshot, is refused: put a matching `swift` on `PATH`.
- Each release installs once per machine: a later task, in any workspace,
  runs the release an earlier one installed.
- On Linux, a toolchain needs system packages, such as `libcurl` and
  `libxml2` headers. Installing them takes root, so the task says which ones
  swiftly names and does not install them.
- The test task of a package appends the directory of its toolchain's
  `llvm-cov` to its `PATH` when that `PATH` holds none, so `swiftpm-test`
  exports the LCOV with the `llvm-cov` of the `swift` that ran the tests.
- With offline dependencies, as in a hosted run, nothing is installed: a
  missing `swift` fails the task with `stacks.toolchain_missing`, and a
  mismatch with `stacks.toolchain_mismatch`.
- An Xcode project keeps its toolchain check only: the extension installs no
  Xcode.

## What reaches the cache key

Each task reads the files of its selector, `swift:lint`, `swift:test` or
`swift:build`, and its component's toolchain file:

| Input set | Package | Xcode project |
|---|---|---|
| `sources` | `Sources/**`, `Plugins/**` | `**`: every file of the project's directory |
| `swiftSources` | | `**/*.swift` |
| `tests` | `Tests/**` | (in `sources`) |
| `manifests` | `Package.swift`, `Package@swift-*.swift` | `*.xcodeproj/project.pbxproj`, `*.xcodeproj/xcshareddata/**` |
| `lintConfig` | `.swift-format`, in the component and at the workspace root | the same |
| `lockfiles` | `Package.resolved` | `*.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved` |

A `**` walk never enters `.build`, `DerivedData`, `SourcePackages`, `Pods`,
`Carthage`, `xcuserdata` or `node_modules`.

A package's `lint` reads `sources`, `manifests` and `lintConfig`; its `test`
reads `sources`, `tests`, `manifests` and `lockfiles`. So a change to a
package's tests reruns its test, not its lint, and a `Package.resolved`
change does not rerun lint. An Xcode project's `lint` reads its Swift files
and `lintConfig`, and lints them all, tests included: a change to a Swift
test file reruns it, a change to any other file does not. Its `test` and
`build` read `sources`, `manifests` and `lockfiles`, so an override of
`sources` keeps the project file and its pins in the key. A lint of `.` leaves
out the hidden entries and the excluded directories.

The key also carries the component's name, its variant and its expected
toolchain version, and the keys of the same task of the local packages it
uses.

## Override the profile

A component overrides any value of its profile in its own `putnami.json`,
under `options["@putnami/swift"]`. An object merges; any other value, a step
list or an input list, replaces the profile's whole. An Xcode project's
override applies after its `xcode` variant.

The overrides a team uses most:

```json
{
  "options": {
    "@putnami/swift": {
      "tasks": {
        "test": {
          "steps": [
            ["{runtime}", "xcodebuild", "test", "--scheme", "ShopCI", "--destination", "platform=iOS Simulator,name=iPhone 16",
             "--result-bundle", "{output}/Tests.xcresult", "--junit", "{output}/junit.xml", "--cobertura", "{output}/coverage.xml"]
          ]
        }
      }
    }
  }
}
```

- **Another scheme or destination**: the `xcodebuild` steps above, with
  `--scheme` and `--destination`. A directory with several projects adds
  `--project Shop.xcodeproj`.
- **A package without a test target**: `"test": {"steps": [["swift", "build",
  "--force-resolved-versions"]], "reports": null}`.
- **Flags for swift test**: `["{runtime}", "swiftpm-test", "--output",
  "{output}", "--", "-c", "release"]`.
- **A lint tool of the team's own**: `"lint": {"steps": [["swiftlint",
  "lint", "--strict"]]}`, with `"lintConfig": ["{root}/.swift-format",
  ".swiftlint.yml"]` in `inputs`.
- **An accepted toolchain difference**: `"toolchain": {"check": "warn"}`.
- **More inputs**: `"inputs": {"sources": ["Sources/**", "Resources/**"]}`.

An override that names an unknown member, changes `id`, `markers` or
`variants`, or adds a stack-root input fails the task with
`stacks.project_config_invalid`.

## Limits

- An app built through an `.xcworkspace`, with CocoaPods for instance, needs
  an override of its steps: the extension builds the `.xcodeproj` and its
  local packages.
- A directory with both a `Package.swift` and an `.xcodeproj` is the package;
  the probe reports `stacks.component_conflict`.
- The scheme defaults to the project's name. A project whose scheme has
  another name overrides it.
- A toolchain file is read in the component's directory only. A
  `.swift-version` or `.xcode-version` at the workspace root is in every
  task's key but is not read.
- A `.swift-format` between the component and the workspace root is not read.
- A package without a name the manifest spells as a literal, at the workspace
  root, is named after the checkout's directory.
- Two Xcode projects whose tests run at the same time may share a simulator.
- The result bundle is a task output, so it is cached with the reports.
- The extension does not install Xcode, a simulator or the system packages a
  Linux Swift needs, and does not archive or sign an app.
- An image holds a static binary only: a package that links a system library
  musl does not offer, or that needs the Foundation of a native Linux build,
  cannot be packaged: it builds its own image.
- An arm64 image is assembled as an amd64 one is; no test runs one.
- With the build layout of Swift 6.4, where a test target builds to
  `<Target>.so` beside `<Target>-test-runner`, the test task finds no
  `*PackageTests.xctest` to export coverage from, and fails after the tests
  pass.
