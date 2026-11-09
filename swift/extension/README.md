# @putnami/swift

`@putnami/swift` runs the Swift packages and Xcode projects of a repository as
Putnami projects: `putnami lint`, `putnami test` and `putnami build` run each
package's `swift` steps and each Xcode project's `xcodebuild` steps with
Putnami's runner and cache, without changing the build. `putnami install` and
`putnami upgrade` resolve and update the packages, and `putnami serve` and
`putnami run` start a package's executable product.

It is a stack extension: a manifest, the Swift stack profile (`profile.json`,
with an `xcode` variant), runtime commands for what an argument vector cannot
do, and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Swift detectors of `putnami observe`.

See [the reference](doc/01-swift.md).
