# How `putnami observe` reads an iOS or Swift repository

Two detectors read a Swift repository: the Swift Package Manager detector
reads each `Package.swift`, and the Xcode detector each `.xcodeproj`. Neither
runs `swift`, `xcodebuild` or `pod`. The change analyses use what they find
to name the changed and dependent components, place tests and coverage, and
diff `Package.resolved` and `Podfile.lock`.

## Swift packages

A directory that holds `Package.swift` is a Swift package. The package is a
component, named by its package name, and so is each of its targets, at the
target's directory and named by the target. Both are of ecosystem `swift` and
source `workspace`. The package depends on each of its targets other than its
test targets, so a change to a target marks its package dependent, and a
change to `Package.swift` itself marks the package changed.

### The export

`Package.swift` is Swift code: only Swift can evaluate it. When the CI writes
the package description beside the manifest, before `putnami observe`, it is
the authority:

```bash
swift package describe --type json > swiftpm.json
```

Run it in each package directory. Each target of the export is a component at
its `path`. Each name in a target's `target_dependencies` is an edge to that
target of the same package. Each name in its `product_dependencies` is an edge
to the local package that declares the product: a `.package(path:)` of the
manifest, or a `fileSystem` dependency of the export. The export names a local
package by its absolute path on the machine that wrote it, so it is matched by
its identity, the lowercased last segment of the path, against the manifest's
paths, else by the trailing segments of the path. A product of a remote
package is no edge. Keep the export out of the commit, as a file the job
writes.

A `swiftpm.json` that does not parse, or lists no target, gets
`observe.swiftpm_export_unreadable`, and the package is read without it.

### Without the export

The static read takes each directory under `Sources` as a target, and each
directory under `Tests` as a test target. Each `.package(path:)` of
`Package.swift`, relative to the package directory, is a local package: each
target under `Sources` depends on it, and each target under `Tests` depends
on every target under `Sources`. The `name:` argument of `Package(` names the
package; comments are left out.

The static read cannot see a target's own dependencies, a custom target
`path:` or a condition, so it errs towards more edges. One info diagnostic,
`observe.swiftpm_static_read`, names the packages read so and the export
command.

## Xcode projects

Each `.xcodeproj` is a component at the directory that holds it, named by the
project, of ecosystem `xcode` and source `manifest`. Its targets are not
components of their own: the project's directory holds the app's sources and
its test targets' directories. Each `XCLocalSwiftPackageReference` of
`project.pbxproj` is an edge to the package at its `relativePath`, relative to
the project's directory, so a change to a local package marks the app
dependent. Remote package references are no edge: `Package.resolved` holds
their versions.

A `project.pbxproj` that does not parse gets `observe.xcode_project_unreadable`,
and the project stays a component with no edge. When a Swift package and an
Xcode project hold one directory, the package wins.

## The walk

The detectors walk the repository. They skip hidden directories, such as
`.build` and `.swiftpm`, and `Pods`, `Carthage`, `DerivedData`,
`SourcePackages` and `node_modules`, and do not enter an `.xcodeproj` or an
`.xcworkspace`. A workspace is not read: the projects and packages it groups
are read on their own.

## Reports

`putnami observe` reads no `.xcresult` bundle. Convert it in the CI:

| Report | Command |
|---|---|
| JUnit | `xcodebuild test ... \| xcbeautify --report junit`, or fastlane `scan` |
| JUnit | `swift test --xunit-output build/reports/xunit.xml` for a package |
| Cobertura | `xcresultparser --output-format cobertura --project-root . Test.xcresult > coverage.xml`, or slather |
| LCOV | `swift test --enable-code-coverage`, then `llvm-cov export -format=lcov` |

These reporters name a test case's class `Module.Class`, such as
`PricingTests.PriceCalculatorTests`, and write no file. The case goes to the
component of the test target named `Module`: a test target of a Swift package,
or a unit or UI test target of an Xcode project. A target name with characters
Swift does not allow in a module name, such as `-`, matches with `_` in their
place. A class name without a module is looked for in every test target. The
case's file is `Class.swift` under the target's directory, else the Swift file
there that declares the class. An Xcode test target's directory is the one
named by the target beside the project, else the project's directory. A
coverage report's files map as in any repository: see
[the change analyses](06-change-analyses.md#tests-and-coverage).

## Lockfiles

A `Package.resolved`, wherever it lies, such as
`App.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/` or a workspace's
`xcshareddata/swiftpm/`, is a lockfile of ecosystem `swift`: each pin by its
identity, at its version, else at its revision for a pin on a branch or a
commit. Format versions 1 to 3 are read; version 1 names a pin by its package.

A `Podfile.lock` is a lockfile of ecosystem `cocoapods`: each entry of `PODS`
by its root pod, so `Firebase/Core` and `Firebase/Analytics` are one package
`Firebase`, at the version in parentheses. A pod that `EXTERNAL SOURCES`
installs from a `:path:` is the repository's own code and is left out.

## Limits

- Package traits, conditional dependencies and plugins are read only through
  the export.
- A CocoaPods pod, a Carthage framework and a remote Swift package are no
  component.
- An Xcode target's own dependencies on another target of its project are not
  read: the project is one component.
