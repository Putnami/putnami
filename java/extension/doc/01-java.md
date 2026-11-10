# Run a Gradle or Maven build with Putnami

`@putnami/java` turns each Gradle project, Android included, and each Maven
module of a Java or Kotlin repository into a Putnami project, and runs its
lint, test and build with the build the team already uses. It reads the
repository the way `putnami observe` does. A machine with no JDK and no Maven
runs it: the extension installs a pinned Eclipse Temurin JDK and Apache Maven
when the machine has none. An application also serves, runs, and ships as an
image. The samples in [`java/samples`](../../samples/README.md) show both
builds.

## Adopt it

1. Declare the extension in `putnami.workspace.json`:

   ```json
   { "extensions": ["@putnami/java"] }
   ```

2. Run `putnami projects sync`. Each Gradle project that `settings.gradle.kts`
   or `settings.gradle` includes with a string literal, the root project when
   it has a `src/` directory, and each module a `pom.xml` lists, with any
   packaging but `pom`, joins the workspace's `includes`. No `putnami.json` is
   needed.
3. Run `putnami lint,test,build`.

| Component | Project name | `{component}`, the build's reference |
|---|---|---|
| Gradle `:libs:core` | `libs/core` | `:libs:core:`, the task path prefix |
| Gradle root project | its directory's name | `:` |
| Maven `com.acme:pricing` | `pricing` | `com.acme:pricing` |

Each project gets an edge to each component it depends on in the same build:
`project(":x")` and `projects.x` in a Gradle build file, a dependency on a
sibling module's coordinates in a POM. A directory that is both a Gradle
project and a Maven module runs as the Gradle project, with the warning
`java.build_conflict`.

A component is an `application` when its build file applies the Gradle
`application` or Spring Boot plugin, in a `plugins {}` block or with
`apply plugin`, or its POM's build applies `spring-boot-maven-plugin` with no
`<skip>true</skip>`. Any other is a `library`, which `serve`, `run` and
`package` leave out: a dependency on Spring Boot makes no application, and an
Android application, which runs on a device, is a library to Putnami.

### Builds in subdirectories

A build need not sit at the workspace root. A Gradle build's root is the
nearest directory that holds `settings.gradle.kts` or `settings.gradle`; a
Maven build's root is the outermost `pom.xml` that lists the module, through a
chain of `pom.xml` files or a `<module>` path such as `services/api`, else the
module's own. That root is the stack root of the build's components: their steps run
its `gradlew` or its `pom.xml`, and the tasks of one build run one at a time.
When the root is no component itself, it is an input-only project its
components depend on, so its settings, wrapper and root POM reach their keys.
A Gradle build inside another one, such as `buildSrc` or an included build, is
part of the build that holds it, and a nested Gradle build the outer one does
not include is left out. A build file under the `src`, `target` or `build`
directory of another build, such as an invoker test or an archetype template,
is no build; a build under a plain `src` directory, such as `src/adservice`,
is. A Gradle plugin declared with `apply false` is not applied.

## The variant of each component

The extension reads each component's build file and gives it a variant: a
part of the profile with that build's steps and reports.

| Variant | When |
|---|---|
| `gradle` | A Gradle project, without the ones below |
| `gradle-jacoco` | Its build file applies the `jacoco` plugin |
| `gradle-platform` | Its build file applies `java-platform`: a project with `check` and `assemble` and no test task |
| `gradle-catalog` | Its build file applies `version-catalog`: a project with neither |
| `android` | Its build file applies an Android plugin (`com.android.application`, `com.android.library`, a `libs.plugins.android.*` alias) or holds an `android { }` block |
| `android-coverage` | An Android project whose build file turns on `enableUnitTestCoverage` (or the older `testCoverageEnabled`) |
| `maven` | A Maven module |

## What each task runs

Each task runs in the component's directory, with no shell. Gradle runs
through the wrapper at the build root, `{root}/gradlew`, with the component's
task path; Maven runs `mvn` with `--also-make`, so the modules a component
depends on are built in the same reactor.

| Variant | `lint` | `test` | `build` |
|---|---|---|---|
| `gradle` | `gradlew {component}check -x {component}test` | `gradlew {component}test` | `gradlew {component}assemble` |
| `gradle-jacoco` | as `gradle` | `gradlew {component}test {component}jacocoTestReport` | as `gradle` |
| `gradle-platform` | `gradlew {component}check` | none | `gradlew {component}assemble` |
| `gradle-catalog` | none | none | `gradlew {component}generateCatalogAsToml` |
| `android` | as `gradle` | `gradlew {component}testDebugUnitTest` | `gradlew {component}assembleDebug` |
| `android-coverage` | as `gradle` | `gradlew {component}testDebugUnitTest {component}createDebugUnitTestCoverageReport` | as `android` |
| `maven` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make verify -DskipTests` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make verify` | `mvn --projects {component} clean`, then `mvn --projects {component} --also-make package -DskipTests` |

Gradle commands also pass `--console=plain`, and Maven ones `--batch-mode` and
`--file {root}/pom.xml`, so the reactor is the whole build.
Maven first cleans the component alone: its compiler recompiles a module only
when a dependency changed in the same run, so a module whose upstream changed
in an earlier task would otherwise pass on its old classes. Only the
component's own `target/` is removed, not the root's, which a Cargo
workspace at the root may share, unless the component is the root module of
a single-module build. The clean also removes what an earlier task
of the component left in `target/`, such as the jar of a `build`; the reports
a task reads are copied out before it ends, so a cache hit restores them.

Maven `lint` runs `verify` without running the tests, so the checks a POM binds to
`validate` or `verify` (Checkstyle, Spotless, PMD, SpotBugs) run. Maven `test`
runs `verify`, so the JaCoCo report a POM binds to `verify` is written. With
`--also-make`, the tests of the modules a component depends on run again in
its `test`; a repository that installs its modules into the local repository
can drop the flag with an override.

## Reports

The test task reads the JUnit reports and the JaCoCo XML report into the test
and coverage summary.

| Variant | JUnit | JaCoCo |
|---|---|---|
| `gradle` | `build/test-results/test/*.xml` | none |
| `gradle-jacoco` | `build/test-results/test/*.xml` | `build/reports/jacoco/test/jacocoTestReport.xml`, when the build turns on its XML report (`reports { xml.required = true }`) |
| `android` | `build/test-results/testDebugUnitTest/*.xml` | none |
| `android-coverage` | `build/test-results/testDebugUnitTest/*.xml` | `build/reports/coverage/test/debug/report.xml` |
| `maven` | `target/surefire-reports/TEST-*.xml`, `target/failsafe-reports/TEST-*.xml` | `target/site/jacoco/jacoco.xml`, when the build writes one |

A component with no file under `src/test` (or `src/testDebug` for Android)
expects no report, so a module without tests passes. Otherwise a failed case,
a step that exits non-zero, or a JUnit report that is missing fails the task,
and so does a missing JaCoCo report for `android-coverage`. The reports are removed before the steps, so a report an
earlier run left is never read as this run's.

## The toolchain check

Before its steps, each task compares the version `javac -version` prints with
the one `.java-version` states, in the component's directory, then at the
stack root. It checks `javac`, so a JRE alone is no JDK.

| Case | Result |
|---|---|
| The versions match | The task runs; `21` matches every 21 release, such as 21.0.4, and keys the major line |
| `javac` is missing, or differs | The task installs the JDK `.java-version` accepts, below, and runs it |
| No pinned JDK matches, such as `.java-version` 11 | The task fails with `stacks.toolchain_mismatch` or `stacks.toolchain_missing`; set `toolchain.check` to `warn` to run the machine's anyway |
| No `.java-version` | The warning `stacks.toolchain_unpinned`: the cache key carries no Java version |

### A machine with no JDK or no Maven

The extension pins three Eclipse Temurin JDKs, 17.0.21, 21.0.13 and 25.0.5,
for Linux and macOS on x64 and arm64 and Windows on x64, each by the SHA-256
the Adoptium API publishes. When `javac` is missing or outside
`.java-version`, the task installs the one `.java-version` accepts, 21 first,
under `toolchains/java` of the Putnami home, and runs it first on `PATH` and
as `JAVA_HOME`, which Gradle and Maven read. Every later task reuses it. The
archive's links, license notices under `legal/`, are not installed.

A Maven component with no `mvn` on `PATH` gets Apache Maven 3.9.11 the same
way, under `toolchains/maven`. A Gradle build runs its wrapper, which
downloads the Gradle version `gradle/wrapper/gradle-wrapper.properties` names;
that file is an input of every Gradle task, so a wrapper upgrade reruns them.
Offline, a task installs nothing.

A build that uses Gradle toolchains (`jvmToolchain(21)`) compiles with the
JDK it names, whatever runs Gradle. Java 8's `1.8.0_392` form matches only an
exact `.java-version`.

## What reaches the cache key

A task's key covers the component's files its task reads, the build-root
files, the component's `putnami.json`, its variant and the expected Java
version, and the key of the same task of each component it depends on.

| Input set | Patterns |
|---|---|
| `sources` | `src/main/**`; for Android also `src/debug/**`, `src/release/**`, `proguard-rules.pro`, `consumer-rules.pro` |
| `tests` | `src/test/**`, `src/testFixtures/**`; for Android also `src/testDebug/**` |
| `gradle` | the build file and `gradle.properties` of the project; at the root the settings and build files, `gradle.properties`, the catalogs and script plugins under `gradle/`, each project's own `*.versions.toml` and `gradle/*.versions.toml`, the wrapper properties, the build files and `src/**` of `buildSrc`, and the build files and `src/**` of every project of `build-logic` |
| `gradleLocks` | `gradle.lockfile`; at the root `settings-gradle.lockfile` and `gradle.lockfile` |
| `maven` | `pom.xml`; every `pom.xml` of the build up to four directories below its root, parents included, and `.mvn/maven.config`, `.mvn/jvm.config`, `.mvn/extensions.xml` at the root. The copy a jar build writes under `target/classes/META-INF/maven` lies deeper and reaches no key |
| `lintConfig` | `lint.xml`, `detekt.yml`; at the root `.editorconfig` and `config/checkstyle/**`, `config/detekt/**`, `config/pmd/**`, `config/spotbugs/**` |

`lint` reads `sources`, `tests`, the build set and `lintConfig`: Gradle's
`check` compiles and checks the test sources (`checkstyleTest`, `pmdTest`,
detekt), and Maven's `-DskipTests` still compiles them and builds test-jars.
`test` reads `sources`, `tests`, the build set and the lockfiles; `build`
reads `sources`, the build set and the lockfiles, and Maven's also `tests`.
So a lockfile change reruns test and build, never lint. A change to any POM
reruns every Maven task: a parent POM's settings reach every module.

## Serve and run an application

`putnami serve` and `putnami run` first build the application's runnable form
with its own build, under the lock the tasks of its build share, copy it into
a private directory, and start the copy in the application's directory on the
task's JDK:

| Application | Built with | Started as |
|---|---|---|
| Gradle `application` plugin | `gradlew {component}installDist` | `build/install/<name>/bin/<name>` |
| Gradle with Spring Boot | `gradlew {component}bootJar` | `java -jar build/libs/<name>.jar` |
| Maven with Spring Boot | `mvn --projects {component} --also-make package -DskipTests` | `java -jar target/<artifact>.jar` |

`serve` sets `PORT` and `SERVER_PORT`, which Spring Boot reads, to its
`--port` flag, 3000 by default, and reports the server ready once the port
accepts connections, so `putnami qualify` requests the routes that
`schema/http-routes.json` lists. It runs until the user stops it. `run` runs
the application once with `--args` and answers its exit code.

## Package and publish an image

`putnami package --docker` assembles the image of an application with the OCI
assembler of the extension SDK, with no Docker daemon: its runnable form under
`/app`, on the Eclipse Temurin JRE of the task's Java major
(`eclipse-temurin:<release>-jre`, pinned by digest for 17, 21 and 25), run as
user 1000 with `PORT` and `SERVER_PORT` set to 8080. The image name is the
project name in lowercase.

```bash
putnami package --docker --docker-load --projects /java/samples/maven/application
putnami publish --docker --projects /java/samples/maven/application
```

| Flag | Meaning |
|---|---|
| `--docker-base-image` | A base pinned by digest, `image@sha256:<digest>`, in place of the Temurin JRE |
| `--docker-tag` | A tag beside the content tag `c-<hash>` |
| `--docker-load` | Also load the image into the local Docker daemon |
| `--platform` | The image platform, `linux/amd64` by default |

An application turns the image on with `"options": { "package": { "docker":
true } }` in its `putnami.json`, or with `"publish": ["docker"]`, which also
declares the image as a member of the release set under the namespace of
`registries.oci.publish`. `putnami publish --docker` pushes it through the
publisher every stack shares, and `--docker-registry <registry>` overrides that
registry for one push. A library packages nothing: it reaches its Maven
repository through its own build.

## Override the profile

A component overrides any value of its variant's profile in its own
`putnami.json`, under `options["@putnami/java"]`. An object merges; any other
value, such as a list of steps or patterns, replaces the variant's whole. The
overrides a team uses most:

An Android app with product flavors runs the unit tests of one flavor:

```json
{
  "options": {
    "@putnami/java": {
      "tasks": {
        "test": {
          "steps": [["{root}/gradlew", "--console=plain", "{component}testFreeDebugUnitTest"]],
          "reports": { "junit": ["build/test-results/testFreeDebugUnitTest/*.xml"], "coverage": [] }
        },
        "build": { "steps": [["{root}/gradlew", "--console=plain", "{component}assembleFreeDebug"]] }
      },
      "inputs": { "sources": ["src/main/**", "src/free/**", "src/debug/**"] }
    }
  }
}
```

A Maven repository with the Maven wrapper runs it instead of `mvn`:

```json
{
  "options": {
    "@putnami/java": {
      "tasks": {
        "test": { "steps": [
          ["{root}/mvnw", "--batch-mode", "--file", "{root}/pom.xml", "--projects", "{component}", "clean"],
          ["{root}/mvnw", "--batch-mode", "--file", "{root}/pom.xml", "--projects", "{component}", "--also-make", "verify"]
        ] }
      }
    }
  }
}
```

A JDK that differs from `.java-version` on purpose:

```json
{ "options": { "@putnami/java": { "toolchain": { "check": "warn" } } } }
```

A steps override keeps the variant's reports: a test step must still write
them where the variant reads them, or the override also replaces
`tasks.test.reports`. An override fails the task with
`stacks.project_config_invalid` when it names an unknown member, changes `id`,
`markers` or `variants`, or adds a build-root input the extension does not
declare. The design of the stack core is in
[its design note](../../../tooling/stacks/doc/01-design.md).

## Limits

- A Gradle composite build (`includeBuild`) runs as the build that includes
  it: its included builds are not projects.
- A Gradle settings file that computes its includes keeps the projects it
  names with string literals, with the warning `observe.gradle_dynamic_settings`.
- A plugin applied from a convention plugin or from the root build file, such
  as `subprojects { apply(plugin = "jacoco") }`, picks no variant: the
  project overrides its test task.
- Gradle builds without a wrapper: override the steps to run `gradle`.
- The build file of an intermediate Gradle project that configures its
  children, such as `libs/build.gradle.kts` for `:libs:core` with a
  `subprojects { }` block, is not in the children's keys: after a change to
  it, a cached result of a child can be stale. Move that configuration to a
  convention plugin under `buildSrc` or `build-logic`, which every key reads,
  or run the children's tasks with `--no-cache` after such a change.
- A Maven module is named by its artifactId: two modules with the same
  artifactId in different groups need a `name` in their `putnami.json`.
- The Maven wrapper `mvnw` is not used unless a project overrides its steps.
- An application built another way, such as a fat jar from the Shadow plugin or
  a Maven module with no Spring Boot, is a library to `serve`, `run` and
  `package`.
- A Maven module is built with `--also-make` by `serve`, `run` and `package`,
  so the modules it depends on are built again.
- An image is built for one platform; the tests run the `linux/amd64` one.
- The image's JRE follows the JDK that ran the build, not a Gradle toolchain
  the build compiles for: a `jvmToolchain(25)` build run on JDK 21 names
  `--docker-base-image` with a Java 25 base.
- A JDK found only through `JAVA_HOME`, with no `javac` on `PATH`, is not
  seen: the task installs one.
- A library is published by its own build, not by `putnami publish`.
