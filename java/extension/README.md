# @putnami/java

`@putnami/java` runs the projects of a Gradle build, Android included, and the
modules of a Maven build as Putnami projects: `putnami lint`, `putnami test`
and `putnami build` run each component's own Gradle or Maven steps with
Putnami's runner and cache, without changing the build.

It is a stack extension: a manifest, the Java stack profile (`profile.json`)
and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Gradle and Maven detectors of `putnami observe`.

See [the reference](doc/01-java.md).
