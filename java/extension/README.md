# @putnami/java

`@putnami/java` runs the projects of a Gradle build, Android included, and the
modules of a Maven build as Putnami projects: `putnami lint`, `putnami test`
and `putnami build` run each component's own Gradle or Maven steps with
Putnami's runner and cache, without changing the build. A machine needs no
JDK, no Gradle and no Maven: `putnami install` installs the pinned ones the
build asks for and fills its dependency caches, and each task installs what it
still lacks. An application also serves, runs, and ships as an image with
`putnami package` and `publish`.

It is a stack extension: a manifest, the Java stack profile (`profile.json`)
and the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md). The components and
their edges come from the Gradle and Maven detectors of `putnami observe`.

See [the reference](doc/01-java.md).
