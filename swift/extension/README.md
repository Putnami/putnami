# @putnami/swift

`@putnami/swift` runs the Swift packages and Xcode projects of a repository as
Putnami projects: `putnami lint`, `putnami test` and `putnami build` run each
package's `swift` steps and each Xcode project's `xcodebuild` steps with
Putnami's runner and cache, without changing the build. `putnami install` and
`putnami upgrade` resolve and update the packages, `putnami serve` and
`putnami run` start a package's executable product, and `putnami package` and
`putnami publish` produce the image of an application, a static binary on a
distroless base. A package task on a machine with no Swift, or outside the
release `.swift-version` states, installs that release with a pinned swiftly
under the Putnami home.

It is a stack extension: a manifest, the Swift stack profile (`profile.json`,
with an `xcode` variant), runtime commands for what an argument vector cannot
do, and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Swift detectors of `putnami observe`.

See [the reference](doc/01-swift.md).
