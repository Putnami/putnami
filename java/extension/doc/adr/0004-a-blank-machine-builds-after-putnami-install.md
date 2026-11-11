# ADR 0004 — A blank machine builds after putnami install

- **Status**: accepted
- **Date**: 2026-11-10
- **Scope**: `@putnami/java`, its toolchain, its Gradle and Maven steps and
  its `workspace-install`
- **Amended by**: [ADR 0005](0005-an-android-build-gets-the-sdk-the-workspace-accepts.md),
  which installs the Android SDK once the workspace accepts its licenses

## Context

[ADR 0001](0001-the-extension-installs-a-pinned-jdk.md) installs the JDK
`.java-version` states and Maven 3.9.11. A Gradle or Maven build still failed
on a machine that holds no JDK, Gradle or Maven:

- A build whose toolchains ask for another JDK (`jvmToolchain(17)`,
  `JavaLanguageVersion.of(17)`) fails: Gradle looks for that JDK among the
  ones it detects on the machine, not under the Putnami home, and downloads
  one only through a toolchain resolver plugin, such as foojay, from Gradle 8
  on.
- `.java-version` 11 had no pinned JDK, and Java 8, whose `javac` prints
  `1.8.0_512`, matched no `.java-version`.
- A Gradle build without a wrapper had nothing to run.
- A Maven build ran 3.9.11 or the machine's `mvn`, whatever its
  `.mvn/wrapper/maven-wrapper.properties` states.
- `putnami install` prepared nothing for a Java build.

## Decision

- **JDKs.** The extension also pins Temurin 11.0.33 for the five platforms,
  and Temurin 8u512 (`1.8.0_512`) for Linux on x64 and arm64 and Windows on
  x64, the platforms Adoptium publishes it for, each by the SHA-256 the
  Adoptium API publishes. The stack's version rule reads Java 8 as both 8 and
  1.8: `8`, `1.8`, `1.8.0` and `8.0.512` accept `1.8.0_512`, and
  `.java-version` is read up to its first character that is neither a digit
  nor a dot, so `1.8.0_512` states `1.8.0`.
- **Gradle toolchains.** Each Gradle step reads the Java versions the build's
  toolchains ask for: `JavaLanguageVersion.of(N)` and `jvmToolchain(N)` in the
  build files of its projects and root, its script plugins under `gradle/`,
  and the sources of `buildSrc` and `build-logic`, as a literal, a
  `JavaVersion` constant or a version of the `libs` catalog, and the daemon
  JVM criteria of `gradle/gradle-daemon-jvm.properties`. It installs the
  pinned JDK of each major other than the one that runs Gradle and passes
  their homes as `-Porg.gradle.java.installations.paths=<homes>`. Gradle
  reads that property from 6.7 on; a build on 7.6, 8.0, 8.14 and 9.8 that
  fails without it compiles with it, and on 9.8 the daemon JVM criteria
  resolve against it too. A command-line property reaches every Gradle step,
  overrides included, with no file written into the repository; the
  installations-from-environment property and an init script would need the
  same argument.
- **Gradle without a wrapper.** A step that names `{root}/gradlew` in a build
  that has none, or `gradle` when none is on `PATH`, runs a pinned Gradle
  distribution, checked by the SHA-256 services.gradle.org publishes: 9.8.2,
  which runs on JDK 17 to 25, or 8.14.6 on an older JDK.
- **Maven's release.** A step that names `mvn` runs the release the
  `distributionUrl` of `.mvn/wrapper/maven-wrapper.properties` names,
  installed under the Putnami home, whatever `mvn` the machine holds. The
  archive is checked against the `distributionSha256Sum` the file states,
  else against the SHA-512, then the SHA-256, its host publishes beside it,
  and refused when there is none. The extension never runs `mvnw`: the script
  downloads and runs code the task cannot check, and a build that needs it
  overrides its steps. Without the file, the `mvn` on `PATH` runs, else
  Maven 3.9.11.
- **The step hook.** These adaptations go through the stack core's
  `Stack.Step`
  ([ADR 0014 of the stack core](../../../../tooling/stacks/doc/adr/0014-a-stack-adapts-each-step-of-a-task.md)),
  and `serve`, `run`, `package` and `workspace-install` run the same
  adaptation.
- **workspace-install.** At each stack root, except the projects
  `disable.tags` excludes: the JDK `.java-version` accepts, the toolchain
  JDKs, then the build tool, the wrapper's distribution through
  `gradlew --version` or the pinned Gradle, or the build's Maven. Then the
  dependency cache: Gradle through an init script that resolves every
  resolvable configuration of every project, leniently, each configuration it
  cannot resolve whole being a warning; Maven through
  `maven-dependency-plugin:3.11.0:go-offline`, best effort, with a warning
  that names what it misses. The force flag refreshes the dependencies.
  Offline it installs nothing: a root whose JDK, Gradle distribution or Maven
  is missing is left out with a warning, and the resolution runs offline.

## Rejected alternatives

- **Run `mvnw`.** It would honor the release too, but it downloads with the
  script's own rules, checks a digest only when the file states one, and
  runs whatever the wrapper jar or script holds.
- **Rely on the foojay resolver.** It needs the build to apply it, and it
  downloads a JDK the extension did not check.
- **Write the toolchain homes into `gradle.properties` or the Gradle user
  home.** It changes files the team owns and every other build of the
  machine.

## Consequences

- A blank machine runs `putnami install`, then `putnami lint,test,build`; the
  tasks also install what they lack, so the install is a warm-up, not a
  prerequisite.
- A toolchain version the build computes another way, such as from
  `gradle.properties`, is not read, and Gradle finds or provisions that JDK
  itself. A toolchain that asks for a vendor other than Adoptium or Eclipse
  does not match the pinned JDKs.
- There is still no `workspace-fetch`: resolving a Gradle build evaluates its
  scripts, and `go-offline` loads the build's extensions.
- The Android SDK, credentials for private repositories and `deps-upgrade`
  stay outside the extension. Amended by
  [ADR 0005](0005-an-android-build-gets-the-sdk-the-workspace-accepts.md): the
  extension installs the Android SDK once the workspace accepts its licenses.
