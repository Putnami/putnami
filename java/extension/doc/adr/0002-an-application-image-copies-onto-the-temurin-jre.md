# ADR 0002 — An application image copies onto the Temurin JRE

- **Status**: accepted
- **Date**: 2026-11-08
- **Scope**: `@putnami/java`, the `serve`, `run`, `package` and `publish` commands

## Context

`@putnami/go`, `@putnami/typescript` and `@putnami/php` assemble their images
with the OCI assembler of the extension SDK: no Docker daemon, files copied
onto a base pinned by digest. A Java application runs on a JRE, from a form
its build produces: the `installDist` directory of the Gradle `application`
plugin, or the executable jar of Spring Boot.

## Decision

- `serve`, `run` and `package` first build the application's runnable form
  with its own build: `gradlew <project>:installDist` or `<project>:bootJar`,
  `mvn --projects <module> --also-make package -DskipTests` for Spring Boot.
  They build under the stack root's lock, as the other tasks of the build do,
  and copy the form aside before they release it, so a concurrent `build`
  that cleans the module never truncates what they run or package.
- An application is a project whose build applies the Gradle `application` or
  Spring Boot plugin, or a Maven module that builds with an active
  `spring-boot-maven-plugin`. An Android application is a library to these
  commands: it runs on a device, not on a JRE.
- `serve` and `run` start that form on the selected JDK. `package` copies it
  onto `eclipse-temurin:<release>-jre` of the Java major the task selected,
  pinned by digest for each JDK the extension pins, or onto a base the project
  pins by digest. The image runs as user 1000, with `PORT` and `SERVER_PORT`
  set to 8080, and starts a jar with the `java` of the base image's `PATH`.
- `publish` shares the publisher of the other stacks, with its registry and
  release-set rules.

## Rejected alternatives

- **A generated Dockerfile run with `docker build`.** It needs a Docker daemon.
- **Jib.** A plugin each build would have to apply, which the extension cannot
  do without changing the build.
- **`gradlew run` and `spring-boot:run` for serve.** Each needs its plugin
  resolvable in every reactor module, and serve would not run what the image
  runs.

## Consequences

- An application built another way, such as a fat jar from the Shadow plugin
  or a Maven module with no Spring Boot, gets no image: it overrides nothing
  and is reported as a library.
- An image built for one platform is not run on another by the tests.
